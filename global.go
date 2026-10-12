package servicehttp

import (
	"sync"
	"sync/atomic"
)

var (
	defaultClient atomic.Pointer[Client]
	named         sync.Map // string -> *Client
)

// Global returns a process-wide client. Without a name it returns the default
// client, creating one with default settings on first use. With a name it
// returns the client registered under that name by [SetGlobalClient], or the
// default client if none is registered. Safe for concurrent use.
//
// Build each named client with its own [New] call to isolate its connection
// pool; clients derived with [Client.With] share their parent's pool.
func Global(name ...string) *Client {
	if len(name) > 0 && name[0] != "" {
		if c, ok := named.Load(name[0]); ok {
			return c.(*Client)
		}
	}
	if c := defaultClient.Load(); c != nil {
		return c
	}
	defaultClient.CompareAndSwap(nil, New())
	return defaultClient.Load()
}

// SetGlobalClient atomically registers c as the default client, or under
// name if given. A nil c resets the default (recreated lazily) or removes the
// named client.
func SetGlobalClient(c *Client, name ...string) {
	switch {
	case len(name) > 0 && name[0] != "" && c == nil:
		named.Delete(name[0])
	case len(name) > 0 && name[0] != "":
		named.Store(name[0], c)
	default:
		defaultClient.Store(c)
	}
}
