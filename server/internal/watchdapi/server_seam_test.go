package watchdapi

import "context"

// Serve is a test helper over the production path, Listen and then
// ServeListener, the two steps watchd's runtime takes apart: it answers the API
// on an owned unix socket until ctx ends.
func Serve(ctx context.Context, path string, src Source) error {
	l, err := Listen(ctx, path)
	if err != nil {
		return err
	}
	defer l.Close()
	return ServeListener(ctx, l, src)
}
