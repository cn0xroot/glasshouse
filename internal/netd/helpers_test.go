package netd

import (
	"bufio"
	"io"
	"net"
	"net/http"
)

// A fresh bufio.Reader per call would lose buffered bytes, so keep one per conn.
var readers = map[net.Conn]*bufio.Reader{}

func bufioReader(c net.Conn) *bufio.Reader {
	if r, ok := readers[c]; ok {
		return r
	}
	r := bufio.NewReader(c)
	readers[c] = r
	return r
}

func bufioReaderFrom(c net.Conn, _ *http.Response) io.Reader { return bufioReader(c) }
