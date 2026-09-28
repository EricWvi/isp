package tunnel

import (
	"context"
	"io"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// Relay copies bytes between client and upstream until both directions finish,
// ctx is canceled, or neither direction has carried data for idle. Traffic in
// either direction keeps the whole relay open, so a one-way stream such as a
// long download is not cut while the other side stays silent. clientReader
// replaces reads from client when the caller already buffered client bytes.
func Relay(ctx context.Context, client net.Conn, clientReader io.Reader, upstream net.Conn, idle time.Duration) {
	closeBoth := func() {
		_ = client.Close()
		_ = upstream.Close()
	}
	stop := context.AfterFunc(ctx, closeBoth)
	defer stop()
	var activity atomic.Int64
	activity.Store(time.Now().UnixNano())
	var wg sync.WaitGroup
	wg.Add(2)
	copyHalf := func(dst net.Conn, src io.Reader) {
		defer wg.Done()
		_, err := io.Copy(dst, &activeReader{reader: src, activity: &activity})
		if err != nil {
			closeBoth()
			return
		}
		if half, ok := dst.(interface{ CloseWrite() error }); ok {
			_ = half.CloseWrite()
		} else {
			_ = dst.Close()
		}
	}
	go copyHalf(upstream, clientReader)
	go copyHalf(client, upstream)
	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	ticker := time.NewTicker(checkInterval(idle))
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			// A write blocked on a peer that stopped reading records no new
			// activity either, so closing here also releases stalled writes.
			if time.Since(time.Unix(0, activity.Load())) >= idle {
				closeBoth()
				<-done
				return
			}
		}
	}
}

func checkInterval(idle time.Duration) time.Duration {
	interval := idle / 2
	if interval > time.Second {
		interval = time.Second
	}
	if interval < time.Millisecond {
		interval = time.Millisecond
	}
	return interval
}

type activeReader struct {
	reader   io.Reader
	activity *atomic.Int64
}

func (r *activeReader) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	if n > 0 {
		r.activity.Store(time.Now().UnixNano())
	}
	return n, err
}
