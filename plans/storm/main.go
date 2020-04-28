package main

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net"
	"time"

	syncc "sync"

	"github.com/ethereum/go-ethereum/metrics"
	"github.com/pkg/errors"
	"github.com/testground/sdk-go/runtime"
	"github.com/testground/sdk-go/sync"
)

var testcases = map[string]runtime.TestCaseFn{
	"storm": storm,
}

var size int

func main() {
	Setup()

	runtime.InvokeMap(testcases)
}

type ListenAddrs struct {
	Addrs []string
}

var PeerTopic = sync.NewTopic("peers", &ListenAddrs{})

func storm(runenv *runtime.RunEnv) error {
	ctx, cancel := context.WithTimeout(context.Background(), 3000*time.Second)
	defer cancel()

	connCount := runenv.IntParam("conn_count")
	connDelayMs := runenv.IntParam("conn_delay_ms")
	connDial := runenv.IntParam("concurrent_dials")
	outgoing := runenv.IntParam("conn_outgoing")
	size = runenv.IntParam("data_size_kb")

	runenv.RecordMessage("running with data_size_kb: %d", size)
	runenv.RecordMessage("running with conn_outgoing: %d", outgoing)
	runenv.RecordMessage("running with conn_count: %d", connCount)
	runenv.RecordMessage("running with conn_delay_ms: %d", connDelayMs)
	runenv.RecordMessage("running with conncurrent_dials: %d", connDial)

	size = size * 1000

	client := sync.MustBoundClient(ctx, runenv)
	defer client.Close()

	if !runenv.TestSidecar {
		return nil
	}

	if err := client.WaitNetworkInitialized(ctx, runenv); err != nil {
		return err
	}

	tcpAddr, err := getSubnetAddr(runenv.TestSubnet)
	if err != nil {
		return err
	}

	mynode := &ListenAddrs{}
	mine := map[string]struct{}{}

	for i := 0; i < connCount; i++ {
		l, err := net.Listen("tcp", tcpAddr.IP.String()+":0")
		if err != nil {
			metrics.GetOrRegisterCounter("listens.err", nil).Inc(1)
			runenv.RecordMessage("error listening: %s", err.Error())
			return err
		}
		defer l.Close()

		runenv.RecordMessage("listening on %s", l.Addr())
		metrics.GetOrRegisterCounter("listens.ok", nil).Inc(1)

		mynode.Addrs = append(mynode.Addrs, l.Addr().String())
		mine[l.Addr().String()] = struct{}{}

		go func() {
			for {
				conn, err := l.Accept()
				if err != nil {
					return
				}

				go handleRequest(runenv, conn)
			}
		}()
	}

	runenv.RecordMessage("my node info: %s", mynode.Addrs)

	_ = client.MustSignalAndWait(ctx, sync.State("listening"), runenv.TestInstanceCount)

	allAddrs, err := shareAddresses(ctx, client, runenv, mynode)
	if err != nil {
		return err
	}

	otherAddrs := []string{}
	for _, addr := range allAddrs {
		if _, ok := mine[addr]; ok {
			continue
		}
		otherAddrs = append(otherAddrs, addr)
	}

	_ = client.MustSignalAndWait(ctx, sync.State("got-other-addrs"), runenv.TestInstanceCount)

	metrics.GetOrRegisterCounter("other.addrs", nil).Inc(int64(len(otherAddrs)))

	sem := make(chan struct{}, connDial) // limit the number of concurrent k8s api calls

	var wg syncc.WaitGroup
	wg.Add(outgoing)

	for outgoing > 0 {
		randomaddrIdx := rand.Intn(len(otherAddrs))

		addr := otherAddrs[randomaddrIdx]

		outgoing--

		sz := size

		go func() {
			defer wg.Done()

			delay := time.Duration(rand.Intn(connDelayMs)) * time.Millisecond
			runenv.RecordMessage("sleeping for: %s", delay)
			<-time.After(delay)

			sem <- struct{}{}

			t := time.Now()
			conn, err := net.DialTimeout("tcp", addr, 30*time.Second)
			if err != nil {
				runenv.RecordFailure(fmt.Errorf("couldnt dial: %s %w", addr, err))
				metrics.GetOrRegisterCounter("dial.fail.count", nil).Inc(1)
				metrics.GetOrRegisterResettingTimer("dial.fail", nil).UpdateSince(t)

				<-sem
				return
			}
			<-sem

			metrics.GetOrRegisterResettingTimer("dial.ok", nil).UpdateSince(t)

			mb := 4 * 1000 // 4kb
			for sz > 0 {
				var data []byte
				if sz <= mb {
					data = make([]byte, sz)
					sz = 0
				} else {
					data = make([]byte, mb)
					sz -= mb
				}
				rand.Read(data)

				n, err := conn.Write(data)
				if err != nil {
					metrics.GetOrRegisterCounter("conn.write.err", nil).Inc(1)
					runenv.RecordFailure(fmt.Errorf("couldnt write to conn: %s %w", addr, err))
					return
				}
				metrics.GetOrRegisterCounter("bytes.sent", nil).Inc(int64(n))
			}
		}()
	}

	wg.Wait()

	runenv.RecordMessage("done writing")
	_ = client.MustSignalAndWait(ctx, sync.State("done writing"), runenv.TestInstanceCount)
	runenv.RecordMessage("done writing after barrier")

	time.Sleep(10 * time.Second)

	return nil
}

func handleRequest(runenv *runtime.RunEnv, conn net.Conn) {
	n := -1
	for n != 0 {
		buf := make([]byte, 1024*4) // 4kb.
		var err error
		n, err = conn.Read(buf)
		if err != nil && err != io.EOF {
			metrics.GetOrRegisterCounter("conn.read.err", nil).Inc(1)
			fmt.Println("Error reading:", err.Error())
		}
		metrics.GetOrRegisterCounter("bytes.read", nil).Inc(int64(n))
		runenv.RecordMessage("read bytes: %d", n)
	}

	conn.Close()
}

func getSubnetAddr(subnet *runtime.IPNet) (*net.TCPAddr, error) {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil, err
	}
	for _, addr := range addrs {
		if ip, ok := addr.(*net.IPNet); ok {
			if subnet.Contains(ip.IP) {
				tcpAddr := &net.TCPAddr{IP: ip.IP}
				return tcpAddr, nil
			}
		} else {
			panic(fmt.Sprintf("%T", addr))
		}
	}
	return nil, fmt.Errorf("no network interface found. Addrs: %v", addrs)
}

func shareAddresses(ctx context.Context, client *sync.Client, runenv *runtime.RunEnv, mynodeInfo *ListenAddrs) ([]string, error) {
	subCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	ch := make(chan *ListenAddrs)
	if _, _, err := client.PublishSubscribe(subCtx, PeerTopic, mynodeInfo, ch); err != nil {
		return nil, errors.Wrap(err, "publish/subscribe failure")
	}

	res := []string{}

	for i := 0; i < runenv.TestInstanceCount; i++ {
		select {
		case info := <-ch:
			runenv.RecordMessage("got info: %d: %s", i, info.Addrs)
			res = append(res, info.Addrs...)
		case <-ctx.Done():
			return nil, ctx.Err()
		}
		metrics.GetOrRegisterCounter("got.info", nil).Inc(1)
	}

	return res, nil
}
