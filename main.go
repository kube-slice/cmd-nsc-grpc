package main

import (
	"context"
	"fmt"
	nscpb "github.com/sumon009838/nsm-sidecar/pkg/nsc/generated/nsc"
	"github.com/vishvananda/netns"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"net"
	"os"
	"os/signal"
	"strings"
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

func hasNSMInterface() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}

	for _, iface := range ifaces {
		if strings.HasPrefix(iface.Name, "nsm") {
			return true
		}
	}
	return false
}

func checkNsmIpPresent(ctx context.Context, cancel context.CancelFunc) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if !hasNSMInterface() {
				fmt.Println("nsm interface not present need to reconnect")
				cancel()
				return
			}
		}
	}
}

func getConnection(serverAddr string) (*grpc.ClientConn, error) {
	var conn *grpc.ClientConn
	var err error
	for {
		conn, err = grpc.NewClient(serverAddr, grpc.WithTransportCredentials(insecure.NewCredentials()))
		if err != nil {
			fmt.Printf("failed to connect to gRPC server (%v), retrying in 1s...\n", err)
			time.Sleep(1 * time.Second)
			continue
		}
		break
	}
	return conn, nil
}

func main() {
	serverAddr := "cmd-nsc-grpc-server.kubeslice-system.svc.cluster.local:50052"
	if os.Getenv("NSC_GRPC_SERVER_ADDR") != "" {
		serverAddr = os.Getenv("NSC_GRPC_SERVER_ADDR")
	}
	fmt.Printf("NETNS_SERVER_ADDR=%s\n", serverAddr)
	retry := 0
	for {
		signalCtx, cancelSignalCtx := signal.NotifyContext(
			context.Background(),
			os.Interrupt,
			syscall.SIGHUP,
			syscall.SIGTERM,
			syscall.SIGQUIT,
		)
		defer cancelSignalCtx()
		// Retry loop until connection succeeds
		conn, err := getConnection(serverAddr)
		if err != nil {
			return
		}
		client := nscpb.NewNSCServiceClient(conn)
		podName := os.Getenv("POD_NAME")
		podNamespace := os.Getenv("MY_POD_NAMESPACE")
		nodeName := os.Getenv("MY_NODE_NAME")
		networkService := os.Getenv("NSM_NETWORK_SERVICES")
		inodeUrl, err := getInodeURL()
		if err != nil || inodeUrl == "" {
			fmt.Println("failed to get inode URL")
			cancelSignalCtx()
			continue
		}
		resp, err := client.DiscoverServer(signalCtx, &nscpb.ClientNode{
			NodeName: nodeName,
		})
		conn.Close()
		if err != nil || resp.Server_Ip == "" {
			fmt.Printf("failed to discover server: %v\n", err)
			continue
		}
		fmt.Printf("discovered server: %s\n", resp.Server_Ip)
		conn, err = getConnection(resp.Server_Ip)
		defer conn.Close()
		if err != nil {
			fmt.Printf("failed to get connection: %v\n", err)
			continue
		}
		client = nscpb.NewNSCServiceClient(conn)
		go checkNsmIpPresent(signalCtx, cancelSignalCtx)
		_, err = client.ProcessPod(signalCtx, &nscpb.PodRequest{
			Name:           podName,
			Namespace:      podNamespace,
			NodeName:       nodeName,
			NetworkService: networkService,
			InodeURL:       inodeUrl,
			RetryCount:     int32(retry),
		})
		select {
		case <-signalCtx.Done():
			fmt.Println("One request completed waiting 1 minute")
			time.Sleep(1 * time.Minute)
		default:
			time.Sleep(1 * time.Second)
			cancelSignalCtx()
			continue
		}
		retry++
	}
}
