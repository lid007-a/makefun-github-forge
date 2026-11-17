// raw.go — a blocking HTTP/1.1 transport for the forge.
//
// net/http's server parks every request on the runtime netpoller: a kevent /
// epoll_wait to wait for the request, a goroutine wake-up through the
// scheduler, then the read and the write. Profiled on this workload that
// wait-and-wake path is about two thirds of the server's CPU, and in a
// sandbox every one of those syscalls is expensive.
//
// This transport keeps the standard library's HTTP parsing (http.ReadRequest
// reads the request line, headers, Content-Length and chunked bodies) and
// replaces only the I/O: each accepted connection is duplicated to a blocking
// *os.File (TCPConn.File), so the connection's goroutine makes one blocking
// read and one blocking write per request — no poller registration, no
// scheduler hand-off. Responses are serialized into a single buffer and sent
// with one write.
//
// It speaks HTTP/1.1 with keep-alive, HEAD, Content-Length and chunked request
// bodies; anything the parser rejects closes the connection, as a server that
// cannot understand a request must.

package main

import (
	"bufio"
	"bytes"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
)

// rawWriter is an http.ResponseWriter that keeps the reply in memory: every
// handler here writes its whole body at once, so nothing streams.
type rawWriter struct {
	header   http.Header
	status   int
	body     bytes.Buffer
	wrote    bool
	dropBody bool // HEAD: the handler still produces the body, the wire does not
}

func newRawWriter(dropBody bool) *rawWriter {
	return &rawWriter{header: make(http.Header, 6), status: 200, dropBody: dropBody}
}

func (w *rawWriter) Header() http.Header { return w.header }

func (w *rawWriter) WriteHeader(status int) {
	if w.wrote {
		return
	}
	w.wrote = true
	w.status = status
}

func (w *rawWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
	}
	return w.body.Write(p)
}

// Flush satisfies http.Flusher: this transport frames the whole reply at the
// end of the request, so there is nothing to push early.
func (w *rawWriter) Flush() {}

// responseBuf is reused across the requests of one connection, so framing a
// reply allocates nothing after the first few.
type responseBuf struct{ b []byte }

func (rb *responseBuf) frame(w *rawWriter, keepAlive bool) []byte {
	full := w.body.Bytes()
	body := full
	if w.dropBody {
		body = nil
	}
	// 1xx, 204 and 304 carry no body and no length.
	noBody := w.status < 200 || w.status == 204 || w.status == 304

	b := rb.b[:0]
	b = append(b, "HTTP/1.1 "...)
	b = strconv.AppendInt(b, int64(w.status), 10)
	b = append(b, ' ')
	b = append(b, statusText(w.status)...)
	b = append(b, '\r', '\n')

	h := w.header
	if !noBody && h.Get("Content-Length") == "" {
		h.Set("Content-Length", strconv.Itoa(len(full)))
	}
	if !keepAlive {
		h.Set("Connection", "close")
	}
	for k, vs := range h {
		for _, v := range vs {
			b = append(b, k...)
			b = append(b, ':', ' ')
			b = append(b, v...)
			b = append(b, '\r', '\n')
		}
	}
	b = append(b, '\r', '\n')
	if len(body) > 0 {
		b = append(b, body...)
	}
	rb.b = b
	return b
}

func statusText(status int) string {
	if t := http.StatusText(status); t != "" {
		return t
	}
	return "Status"
}

// serveRaw accepts connections and gives each its own goroutine, which blocks
// a thread on the socket instead of registering it with the poller.
func serveRaw(ln net.Listener, handler http.Handler) error {
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return err
		}
		tcp, ok := conn.(*net.TCPConn)
		if !ok {
			conn.Close()
			continue
		}
		// The dup is in blocking mode, so reads and writes on it are plain
		// syscalls; the accepted conn itself is closed straight away.
		f, err := tcp.File()
		if err != nil {
			conn.Close()
			continue
		}
		conn.Close()
		wg.Add(1)
		go func() {
			defer wg.Done()
			serveRawConn(f, handler)
		}()
	}
}

// rawConnBufSize is the read buffer: one request header block plus the start
// of its body usually fits in one read.
const rawConnBufSize = 16 << 10

func serveRawConn(f *os.File, handler http.Handler) {
	defer f.Close()
	br := bufio.NewReaderSize(f, rawConnBufSize)
	rb := &responseBuf{b: make([]byte, 0, 2048)}
	for {
		req, err := http.ReadRequest(br)
		if err != nil {
			return
		}
		keepAlive := !req.Close
		w := newRawWriter(req.Method == "HEAD")
		serveOne(handler, w, req)
		if _, err := f.Write(rb.frame(w, keepAlive)); err != nil {
			return
		}
		// Leave the stream at the next request's boundary even when a handler
		// did not read the body it was given.
		if req.Body != nil {
			io.Copy(io.Discard, req.Body)
			req.Body.Close()
		}
		if !keepAlive {
			return
		}
	}
}

// serveOne runs the handler and turns a panic into the 400 the net/http path
// answers, so a bad request never takes the process down.
func serveOne(handler http.Handler, w *rawWriter, req *http.Request) {
	defer func() {
		if rec := recover(); rec != nil {
			msg := "Bad Request"
			if err, ok := rec.(error); ok {
				msg = err.Error()
			} else if s, ok := rec.(string); ok {
				msg = s
			}
			w.header = make(http.Header, 3)
			w.body.Reset()
			w.header.Set("Content-Type", "application/json")
			w.header.Set("X-Github-Request-Id", "req-panic")
			w.status = 400
			w.wrote = true
			w.body.WriteString(`{"message":` + strconv.Quote(msg) + `}`)
		}
	}()
	handler.ServeHTTP(w, req)
}
