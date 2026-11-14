package proxy

import (
	"context"
	"errors"
	"net"
	"net/http"
	"time"
)

// Serve runs srv on ln until ctx is cancelled, then shuts down gracefully:
// stop accepting, wait for in-flight requests up to timeout, then return.
func Serve(ctx context.Context, srv *http.Server, ln net.Listener, timeout time.Duration) error {
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	sctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	err := srv.Shutdown(sctx)
	if serr := <-errc; serr != nil && !errors.Is(serr, http.ErrServerClosed) && err == nil {
		err = serr
	}
	return err
}
