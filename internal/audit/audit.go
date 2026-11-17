// Package audit writes a tamper-evident JSON Lines log. Every record carries
// the SHA-256 of the previous record, so editing or deleting a line breaks the
// chain. The log lives on the host, outside anything the guest can reach.
package audit

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"
)

type Record struct {
	Seq    uint64         `json:"seq"`
	Time   string         `json:"time"`
	Type   string         `json:"type"`
	Fields map[string]any `json:"fields,omitempty"`
	Prev   string         `json:"prev"`
	Hash   string         `json:"hash"`
}

// Writer is what other packages depend on, so tests can substitute it.
type Writer interface {
	Write(typ string, fields map[string]any) error
}

type Logger struct {
	mu   sync.Mutex
	f    *os.File
	seq  uint64
	prev string
	now  func() time.Time
}

const genesis = "0000000000000000000000000000000000000000000000000000000000000000"

// Open appends to path, continuing the existing chain. A log that does not
// verify is refused: the caller must treat that as "fail closed".
func Open(path string) (*Logger, error) {
	l := &Logger{prev: genesis, now: time.Now}
	if _, err := os.Stat(path); err == nil {
		n, last, err := verifyFile(path)
		if err != nil {
			return nil, fmt.Errorf("existing audit log is corrupt: %w", err)
		}
		l.seq, l.prev = n, last
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	l.f = f
	return l, nil
}

func (l *Logger) Write(typ string, fields map[string]any) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return errors.New("audit log closed")
	}
	r := Record{Seq: l.seq + 1, Time: l.now().UTC().Format(time.RFC3339Nano), Type: typ, Fields: fields, Prev: l.prev}
	h, err := hashRecord(r)
	if err != nil {
		return err
	}
	r.Hash = h
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if _, err := l.f.Write(append(b, '\n')); err != nil {
		return err
	}
	if err := l.f.Sync(); err != nil {
		return err
	}
	l.seq, l.prev = r.Seq, r.Hash
	return nil
}

func (l *Logger) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	err := l.f.Close()
	l.f = nil
	return err
}

// hashRecord hashes everything except the hash itself. encoding/json sorts map
// keys, so the encoding is deterministic.
func hashRecord(r Record) (string, error) {
	r.Hash = ""
	b, err := json.Marshal(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// Verify checks the whole chain and returns the number of records.
func Verify(path string) (uint64, error) {
	n, _, err := verifyFile(path)
	return n, err
}

func verifyFile(path string) (uint64, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	prev := genesis
	var n uint64
	for sc.Scan() {
		var r Record
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil {
			return n, prev, fmt.Errorf("record %d: %w", n+1, err)
		}
		if r.Seq != n+1 {
			return n, prev, fmt.Errorf("record %d: sequence gap (found %d)", n+1, r.Seq)
		}
		if r.Prev != prev {
			return n, prev, fmt.Errorf("record %d: chain broken", r.Seq)
		}
		want, err := hashRecord(r)
		if err != nil {
			return n, prev, err
		}
		if r.Hash != want {
			return n, prev, fmt.Errorf("record %d: hash mismatch", r.Seq)
		}
		prev = r.Hash
		n++
	}
	return n, prev, sc.Err()
}

// Discard is a Writer that drops records (tests only).
type Discard struct{}

func (Discard) Write(string, map[string]any) error { return nil }
