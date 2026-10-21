package tools

import (
	"context"
	"fmt"
	"io"
	"os"
)

// openRegular checks the opened descriptor, not just its path: a file can
// be replaced between Stat and Open. Never truncate before this check.
func (t target) openRegular(ctx context.Context, flags int) (*os.File, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := t.root.OpenFile(t.rel, flags|nonblock, 0o644)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("%s is not a regular file", t.abs)
	}
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	return f, nil
}

// readFile is always bounded, even if the file grows after Stat: an unbounded
// read of a huge workspace file could exhaust memory before permission is asked.
func (t target) readFile(ctx context.Context, limit int64) ([]byte, error) {
	f, err := t.openRegular(ctx, os.O_RDONLY)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	data, err := io.ReadAll(io.LimitReader(f, limit+1))
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if int64(len(data)) > limit {
		return nil, fmt.Errorf("%s is too large (max %d bytes)", t.abs, limit)
	}
	return data, err
}

func (t target) writeFile(ctx context.Context, data []byte) error {
	f, err := t.openRegular(ctx, os.O_WRONLY|os.O_CREATE)
	if err != nil {
		return err
	}
	defer f.Close()
	stop := context.AfterFunc(ctx, func() { _ = f.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	// Once truncation starts the write completes: cancelling it halfway
	// would leave a truncated file. A regular-file write does not block.
	if !stop() {
		return ctx.Err()
	}
	if err := f.Truncate(0); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Close()
}
