// memmeasure measures the memory both xerotty processes spend on
// scrollback, without a display. It attaches to a TEST daemon as the
// GUI does (clientproto + daemonsource.Hub in windowed mode, i.e. the
// GUI's data path without the renderer), opens tabs, replays a
// recorded output file into each one, and then prints its own memory
// reading and the daemon's (get_heap_report on the daemon's MCP
// socket), each with a heap profile.
//
// Never point it at a daemon holding real tabs: it creates tabs and
// types into them.
//
//	go run ./tools/memmeasure -socket /path/test.sock -mcp-socket /path/test.sock.mcp.sock \
//	    -file recorded.txt -tabs 10 -cols 186 -rows 50
package main

import (
	"bufio"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"time"

	"github.com/LXXero/xerotty/internal/clientproto"
	"github.com/LXXero/xerotty/internal/daemonsource"
	"github.com/LXXero/xerotty/internal/memreport"
)

func main() {
	sock := flag.String("socket", "", "test daemon wire socket (required)")
	mcpSock := flag.String("mcp-socket", "", "test daemon MCP socket (for its heap report)")
	file := flag.String("file", "", "recorded output to replay with cat (required)")
	tabs := flag.Int("tabs", 10, "tabs to open")
	cols := flag.Int("cols", 186, "columns")
	rows := flag.Int("rows", 50, "rows")
	want := flag.Int("want-rows", 0, "scrollback rows each tab must reach before measuring")
	profDir := flag.String("profile-dir", "", "directory for this process's heap profile")
	timeout := flag.Duration("timeout", 3*time.Minute, "give up after this long")
	flag.Parse()
	if *sock == "" || *file == "" {
		flag.Usage()
		os.Exit(2)
	}

	cli, err := clientproto.Dial(*sock)
	if err != nil {
		log.Fatalf("dial: %v", err)
	}
	defer cli.Close()
	if _, err := cli.Hello("memmeasure"); err != nil {
		log.Fatalf("hello: %v", err)
	}
	go cli.Run()
	if err := cli.Attach("", false); err != nil {
		log.Fatalf("attach: %v", err)
	}
	<-cli.Attached()
	hub := daemonsource.NewHub(cli)
	hub.SetScrollbackWindowed()
	defer hub.Stop()

	var srcs []*daemonsource.Source
	for i := 0; i < *tabs; i++ {
		s, err := hub.NewTab(*cols, *rows, "")
		if err != nil {
			log.Fatalf("new tab: %v", err)
		}
		srcs = append(srcs, s)
	}
	time.Sleep(time.Second) // let the shells start
	for _, s := range srcs {
		if _, err := s.Write([]byte("cat " + *file + "\r")); err != nil {
			log.Fatalf("write: %v", err)
		}
	}

	// Wait until every tab has the wanted rows and has stopped growing.
	deadline := time.Now().Add(*timeout)
	last, stable, done := -1, 0, false
	for time.Now().Before(deadline) {
		time.Sleep(500 * time.Millisecond)
		sum, min := 0, int(^uint(0)>>1)
		for _, s := range srcs {
			n := s.ScrollbackLen()
			sum += n
			if n < min {
				min = n
			}
		}
		if sum == last && min >= *want {
			if stable++; stable >= 6 {
				done = true
				break
			}
		} else {
			stable = 0
		}
		last = sum
	}
	if !done {
		log.Fatalf("timed out after %s: tabs did not all reach %d scrollback rows and stop growing", *timeout, *want)
	}
	perTab := make([]int, len(srcs))
	for i, s := range srcs {
		perTab[i] = s.ScrollbackLen()
	}

	out := map[string]any{"tabs": *tabs, "cols": *cols, "rows": *rows, "scrollback_rows_per_tab": perTab}
	rep, err := memreport.ReportWithProfile("gui-data-path", *profDir, true, true)
	if err != nil {
		log.Fatalf("own report: %v", err)
	}
	out["client"] = rep
	if *mcpSock != "" {
		d, err := daemonHeap(*mcpSock)
		if err != nil {
			log.Fatalf("daemon report: %v", err)
		}
		out["daemon"] = d
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	_ = enc.Encode(out)
	for _, s := range srcs {
		s.Close()
	}
}

// daemonHeap asks the daemon's MCP socket for its heap report with a
// profile (newline-delimited JSON-RPC).
func daemonHeap(sock string) (json.RawMessage, error) {
	c, err := net.Dial("unix", sock)
	if err != nil {
		return nil, err
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(60 * time.Second))
	if _, err := fmt.Fprintln(c, `{"jsonrpc":"2.0","id":1,"method":"server/heap","params":{"profile":true}}`); err != nil {
		return nil, err
	}
	r := bufio.NewReaderSize(c, 1<<20)
	line, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	var resp struct {
		Result json.RawMessage `json:"result"`
		Error  json.RawMessage `json:"error"`
	}
	if err := json.Unmarshal(line, &resp); err != nil {
		return nil, err
	}
	if len(resp.Error) > 0 {
		return nil, fmt.Errorf("daemon: %s", resp.Error)
	}
	return resp.Result, nil
}
