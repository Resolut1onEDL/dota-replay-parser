package main

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// far more than the loopback socket buffers hold, so a write to a client that does not read blocks
const unreadResponseBytes = 64 << 20

// 2026-10-31: Fly suspended a machine mid-request and its proxy dropped the
// connection while the VM was frozen. After the wake the parse finished and
// the response write went into a dead connection (no RST): the kernel retried
// for ~15 min and the handler — with the only parse slot — never returned.
// A client that stops reading is the same write that never completes.
func TestParsedResponseGivesUpOnAClientThatStopsReading(t *testing.T) {
	t.Run("respondParsed", func(t *testing.T) {
		checkWriteGivesUp(t, func(w http.ResponseWriter) {
			respondParsed(w, make([]byte, unreadResponseBytes), time.Second, 0, "stored")
		})
	})
	t.Run("serveParsed", func(t *testing.T) {
		lifecycleSetup(t, 1)
		writeParser(t, fmt.Sprintf("head -c %d /dev/zero", unreadResponseBytes))
		checkWriteGivesUp(t, func(w http.ResponseWriter) { serveParsed(t.Context(), w, "unused.dem", 0) })
	})
}

func checkWriteGivesUp(t *testing.T, respond func(http.ResponseWriter)) {
	t.Helper()
	old := responseWriteTimeout
	responseWriteTimeout = 300 * time.Millisecond
	t.Cleanup(func() { responseWriteTimeout = old })

	returned := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		defer close(returned)
		respond(w)
	}))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	fmt.Fprintf(conn, "POST /parse-valve HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\n\r\n")
	// the status line only, then the client stops reading
	if _, err := bufio.NewReader(conn).ReadString('\n'); err != nil {
		t.Fatal(err)
	}

	select {
	case <-returned:
	case <-time.After(lifecycleWait):
		t.Fatal("the handler is still writing to a client that stopped reading: the parse slot stays held")
	}
}
