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

// hasNSMInterface reports whether this pod has a usable NSM datapath.
//
// Interface presence alone is not enough. When the peer end of the connection dies -- an nsmgr
// restart, a vl3 router restart, a heal that never completed -- the veth in this pod survives with
// its address still configured while the router side is gone. The connection is then half open:
// nothing routes, but a presence-only check is satisfied and never asks for a reconnect, so the pod
// stays dead until something restarts it. A slice gateway left in that state strands the vl3
// router with a nexthop it cannot resolve.
//
// Require the interface to be up and to still carry an address, which is what a live connection
// looks like and what a torn-down one loses.
func hasNSMInterface() bool {
	ifaces, err := net.Interfaces()
	if err != nil {
		return false
	}

	for _, iface := range ifaces {
		if !strings.HasPrefix(iface.Name, "nsm") {
			continue
		}
		if iface.Flags&net.FlagUp == 0 {
			fmt.Printf("nsm interface %s is down\n", iface.Name)
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			fmt.Printf("cannot read addresses of %s: %v\n", iface.Name, err)
			continue
		}
		for _, addr := range addrs {
			ipNet, ok := addr.(*net.IPNet)
			if !ok || ipNet.IP.To4() == nil {
				continue
			}
			if ipNet.IP.IsUnspecified() || ipNet.IP.IsLoopback() {
				continue
			}
			return true
		}
		fmt.Printf("nsm interface %s has no usable IPv4 address\n", iface.Name)
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

// reconnectDelay paces reconnect attempts. The pod is cut off from the slice until the next
// attempt succeeds, so this is deliberately short.
const reconnectDelay = time.Second

func main() {
	serverAddr := "cmd-nsc-grpc-server.kubeslice-system.svc.cluster.local:50052"
	if os.Getenv("NSC_GRPC_SERVER_ADDR") != "" {
		serverAddr = os.Getenv("NSC_GRPC_SERVER_ADDR")
	}
	fmt.Printf("NETNS_SERVER_ADDR=%s\n", serverAddr)

	// One signal context for the whole process. Each connection attempt gets a child context
	// that the nsm0 watchdog cancels, which lets us tell "the datapath went away, reconnect
	// now" apart from "we are shutting down".
	rootCtx, stopSignals := signal.NotifyContext(
		context.Background(),
		os.Interrupt,
		syscall.SIGHUP,
		syscall.SIGTERM,
		syscall.SIGQUIT,
	)
	defer stopSignals()

	retry := 0
	for rootCtx.Err() == nil {
		signalCtx, cancelSignalCtx := context.WithCancel(rootCtx)
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
		cancelSignalCtx()
		if rootCtx.Err() != nil {
			// Shutting down: nothing to reconnect to.
			return
		}
		// ProcessPod returned, so this pod currently has no NSM datapath -- either the
		// watchdog saw nsm0 go away or the server released the connection. Waiting here is
		// downtime, so retry promptly rather than sitting out a fixed delay.
		select {
		case <-rootCtx.Done():
			return
		case <-time.After(reconnectDelay):
		}
		retry++
	}
}
