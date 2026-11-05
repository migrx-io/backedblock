// Command ui-mock serves the bbctl ui dashboard on a made-up cluster, to see
// the page at a size no dev cluster has. It needs no AWS and no config, and
// any password logs in. It is for developing the page, not part of bbctl:
//
//	go run ./cmd/ui-mock --pools 3 --nodes 5
package main

import (
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/migrx-io/backedblock/internal/dash"
)

func main() {
	var size dash.MockSize
	flag.IntVar(&size.Pools, "pools", 50, "pools in the mock cluster")
	flag.IntVar(&size.Nodes, "nodes", 20, "nodes per pool")
	flag.IntVar(&size.Volumes, "volumes", 30, "volumes per node")
	flag.IntVar(&size.Snapshots, "snapshots", 30, "snapshots per node")
	port := flag.Int("port", 8080, "local port for the dashboard")
	flag.Parse()
	if size.Pools < 1 || size.Nodes < 1 || size.Volumes < 0 || size.Snapshots < 0 {
		log.Fatal("a mock cluster needs at least one pool and one node")
	}

	cfg, stop, err := dash.StartMock(size)
	if err != nil {
		log.Fatal(err)
	}
	defer stop()
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(*port)))
	if err != nil {
		log.Fatal(err)
	}
	fmt.Printf("mock dashboard (%d pools of %d nodes, %d volumes and %d snapshots each) at http://%s, any password (Ctrl-C to stop)\n",
		size.Pools, size.Nodes, size.Volumes, size.Snapshots, ln.Addr())
	srv := &http.Server{Handler: dash.New(cfg).Handler(), ReadHeaderTimeout: 10 * time.Second}
	log.Fatal(srv.Serve(ln))
}
