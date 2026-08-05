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
	pid := os.Getpid()

	// os.NewFile takes ownership of the descriptor, so podNS must not be
	// closed as well: closing it twice would, on the second call, close
	// whatever descriptor number has been recycled in the meantime.
	file := os.NewFile(uintptr(podNS), fmt.Sprintf("/proc/%d/ns/net", pid))
	if file == nil {
		_ = podNS.Close()
		return "", fmt.Errorf("cannot open the pod network namespace as a file")
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

// The pod no longer watches its own interface.
//
// It used to: a ticker checked every 10s that nsm0 was present with a usable
// address, and cancelled the session when it was not, which made the broker
// tear the connection down and build a new one. That cost a new address and
// seconds of downtime for every transient break, and it could cancel a
// connection that was still being set up -- the check was armed before the
// request that creates the interface, so a setup slower than the interval was
// killed and retried forever.
//
// Repair belongs to the broker, which heals the connection in place. What is
// left here is re-attachment: if the broker restarts, its heal state dies with
// it, and only a new ProcessPod call can bring this pod back.

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

// waitBeforeRetry paces reconnect attempts. It reports false when the process
// is shutting down. Every failing path in the loop goes through it: without a
// delay, a broker that is briefly unreachable turns every sidecar on the node
// into a spin loop at exactly the moment the node is already struggling.
func waitBeforeRetry(rootCtx context.Context) bool {
	select {
	case <-rootCtx.Done():
		return false
	case <-time.After(reconnectDelay):
		return true
	}
}

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
			fmt.Printf("failed to get inode URL: %v\n", err)
			cancelSignalCtx()
			waitBeforeRetry(rootCtx)
			continue
		}
		resp, err := client.DiscoverServer(signalCtx, &nscpb.ClientNode{
			NodeName: nodeName,
		})
		conn.Close()
		if err != nil || resp.Server_Ip == "" {
			fmt.Printf("failed to discover server: %v\n", err)
			cancelSignalCtx()
			waitBeforeRetry(rootCtx)
			continue
		}
		fmt.Printf("discovered server: %s\n", resp.Server_Ip)
		conn, err = getConnection(resp.Server_Ip)
		if err != nil {
			fmt.Printf("failed to get connection: %v\n", err)
			cancelSignalCtx()
			waitBeforeRetry(rootCtx)
			continue
		}
		client = nscpb.NewNSCServiceClient(conn)
		_, err = client.ProcessPod(signalCtx, &nscpb.PodRequest{
			Name:           podName,
			Namespace:      podNamespace,
			NodeName:       nodeName,
			NetworkService: networkService,
			InodeURL:       inodeUrl,
			RetryCount:     int32(retry),
		})
		cancelSignalCtx()
		// Closed here rather than deferred: main never returns, so a deferred
		// close would hold every channel this loop has ever opened.
		conn.Close()
		if rootCtx.Err() != nil {
			// Shutting down: nothing to reconnect to.
			return
		}
		// ProcessPod returned, so the broker is no longer holding this pod's
		// session -- it restarted, or it released the connection. Nothing else
		// can bring the pod back, so ask again promptly.
		if !waitBeforeRetry(rootCtx) {
			return
		}
		retry++
	}
}
