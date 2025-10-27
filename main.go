package main

import (
	"context"
	"fmt"
	nscpb "github.com/sumon009838/nsm-sidecar/pkg/nsc/generated/nsc"
	"github.com/vishvananda/netns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func getInodeURL() (string, error) {
	podNS, err := netns.Get()
	if err != nil {
		fmt.Printf("failed to get current pod netns: %v\n", err)
		return "", err
	}
	defer podNS.Close()
	pid := os.Getpid()

	file := os.NewFile(uintptr(podNS), fmt.Sprintf("/proc/%d/ns/net", pid))
	if file == nil {
		fmt.Println("failed to create os.File from NsHandle")
		return "", err
	}
	defer file.Close()

	stat := &syscall.Stat_t{}
	if err := syscall.Fstat(int(file.Fd()), stat); err != nil {
		fmt.Printf("failed to fstat pod ns fd: %v\n", err)
		return "", err
	}
	inode := stat.Ino

	inodeURL := fmt.Sprintf("inode://4/%d", inode)
	return inodeURL, nil
}
func main() {
	serverAddr := "cmd-nsc-grpc-server.kubeslice-system.svc.cluster.local:50052"
	fmt.Printf("NETNS_SERVER_ADDR=%s\n", serverAddr)
	signalCtx, cancelSignalCtx := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer cancelSignalCtx()
	var conn *grpc.ClientConn
	var err error
	for {
		// Retry loop until connection succeeds
		for {
			conn, err = grpc.NewClient(serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
			if err != nil {
				fmt.Printf("failed to connect to gRPC server (%v), retrying in 2s...\n", err)
				time.Sleep(1 * time.Second)
				continue
			}
			fmt.Println("Connected to gRPC server!")
			break
		}
		defer conn.Close()

		client := nscpb.NewNSCServiceClient(conn)

		podName := os.Getenv("POD_NAME")
		podNamespace := os.Getenv("MY_POD_NAMESPACE")
		nodeName := os.Getenv("MY_NODE_NAME")
		networkService := os.Getenv("NSM_NETWORK_SERVICES")
		inodeUrl, err := getInodeURL()
		if err != nil || inodeUrl == "" {
			fmt.Println("failed to get inode URL")
			continue
		}
		_, err = client.ProcessPod(signalCtx, &nscpb.PodRequest{
			Name:           podName,
			Namespace:      podNamespace,
			NodeName:       nodeName,
			NetworkService: networkService,
			InodeURL:       inodeUrl,
		})
		if err != nil {
			select {
			case <-signalCtx.Done():
				fmt.Println("Exiting...")
				break
			default:
				fmt.Printf("failed to process pod (%v), retrying...\n", err)
				continue
			}
		}
		break
	}
}
