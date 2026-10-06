package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const (
	chunkSize = 64 << 20
	// measured on a 923 Mbit/s host: 16 connections give 85 MB/s, 32 give 100-114, 48 are no faster
	connections = 32
	chunkTries  = 8
)

// download fetches url into path over parallel ranged requests, then checks the SHA256. A file that is already
// there passed that check before (it is renamed into place only after it) and is kept.
func download(ctx context.Context, client *http.Client, url, path string, f File, got *atomic.Int64) error {
	if info, err := os.Stat(path); err == nil && info.Size() == f.Size {
		got.Add(f.Size)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	part := path + ".part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	defer out.Close()
	if err := out.Truncate(f.Size); err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	offsets := make(chan int64)
	go func() {
		defer close(offsets)
		for off := int64(0); off < f.Size; off += chunkSize {
			select {
			case offsets <- off:
			case <-ctx.Done():
				return
			}
		}
	}()
	var wg sync.WaitGroup
	var once sync.Once
	var failed error
	for range connections {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for off := range offsets {
				if err := chunk(ctx, client, url, out, off, min(off+chunkSize, f.Size), got); err != nil {
					once.Do(func() { failed = err; cancel() })
					return
				}
			}
		}()
	}
	wg.Wait()
	if failed != nil {
		return failed
	}

	sum := sha256.New()
	if _, err := io.Copy(sum, io.NewSectionReader(out, 0, f.Size)); err != nil {
		return err
	}
	if have := hex.EncodeToString(sum.Sum(nil)); have != f.SHA256 {
		os.Remove(part)
		return fmt.Errorf("SHA256 не совпал: %s вместо %s", have, f.SHA256)
	}
	out.Close()
	return os.Rename(part, path)
}

// chunk downloads bytes [from, to) and retries a failed or stalled request from where it stopped.
func chunk(ctx context.Context, client *http.Client, url string, out io.WriterAt, from, to int64, got *atomic.Int64) error {
	var last error
	for try := 0; try < chunkTries && from < to; try++ {
		if try > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(try) * 2 * time.Second):
			}
		}
		n, err := func() (int64, error) {
			// a chunk slower than ~100 KB/s is a stalled connection
			ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return 0, err
			}
			req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", from, to-1))
			resp, err := client.Do(req)
			if err != nil {
				return 0, err
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusPartialContent {
				return 0, fmt.Errorf("HTTP %d на запрос диапазона", resp.StatusCode)
			}
			return io.Copy(io.NewOffsetWriter(out, from), io.TeeReader(io.LimitReader(resp.Body, to-from), counter{got}))
		}()
		from += n
		if last = err; n > 0 {
			try = 0 // progress: this connection works, the count of attempts starts over
		}
	}
	switch {
	case from >= to:
		return nil
	case last == nil:
		return fmt.Errorf("сервер закрывает соединение раньше конца диапазона")
	}
	return last
}

type counter struct{ n *atomic.Int64 }

func (c counter) Write(p []byte) (int, error) {
	c.n.Add(int64(len(p)))
	return len(p), nil
}
