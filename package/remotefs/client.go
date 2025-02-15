package remotefs

import (
	"context"
	"encoding/gob"
	"io/fs"
	"net/rpc"
	"strings"

	"github.com/livebud/bud/internal/virtual"
	"github.com/livebud/bud/package/socket"
)

func init() {
	gob.Register(&virtual.File{})
	gob.Register(&virtual.Dir{})
	gob.Register(&virtual.DirEntry{})
}

func Dial(ctx context.Context, addr string) (*Client, error) {
	conn, err := socket.Dial(ctx, addr)
	if err != nil {
		return nil, err
	}
	return &Client{rpc.NewClient(conn)}, nil
}

type Client struct {
	rpc *rpc.Client
}

var _ fs.FS = (*Client)(nil)
var _ fs.ReadDirFS = (*Client)(nil)

func (c *Client) Open(name string) (fs.File, error) {
	vfile := new(fs.File)
	if err := c.rpc.Call("remotefs.Open", name, vfile); err != nil {
		if isNotExist(err) {
			return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
		}
		return nil, err
	}
	return *vfile, nil
}

func (c *Client) ReadDir(name string) (des []fs.DirEntry, err error) {
	vdes := new([]fs.DirEntry)
	err = c.rpc.Call("remotefs.ReadDir", name, &vdes)
	if err != nil {
		if isNotExist(err) {
			return nil, &fs.PathError{Op: "readdir", Path: name, Err: fs.ErrNotExist}
		}
		return nil, err
	}
	return *vdes, nil
}

func (c *Client) Close() error {
	return c.rpc.Close()
}

// isNotExist is needed because the error has been serialized and passed between
// processes so errors.Is(err, fs.ErrNotExist) no longer is true.
func isNotExist(err error) bool {
	return strings.HasSuffix(err.Error(), fs.ErrNotExist.Error())
}
