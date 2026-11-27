package server

import "sync"

// siteLocks hands out one mutex per site name, so publishing, switching, pruning and
// deleting one site never wait for another site's slow (for example S3) operations. An
// entry exists only while somebody holds or waits for it.
type siteLocks struct {
	mu sync.Mutex
	m  map[string]*siteLock
}

type siteLock struct {
	sync.Mutex
	refs int
}

// lock takes the lock of the named site and returns the function that releases it.
func (l *siteLocks) lock(name string) (unlock func()) {
	l.mu.Lock()
	if l.m == nil {
		l.m = map[string]*siteLock{}
	}
	e := l.m[name]
	if e == nil {
		e = &siteLock{}
		l.m[name] = e
	}
	e.refs++
	l.mu.Unlock()

	e.Lock()
	return func() {
		e.Unlock()
		l.mu.Lock()
		if e.refs--; e.refs == 0 {
			delete(l.m, name)
		}
		l.mu.Unlock()
	}
}

// reservations tracks the bytes of deploys that passed their quota check but have not yet
// been recorded, so that two deploys of different sites of one owner cannot both fit into
// the same headroom while their uploads are still running. Guarded by Server.quotaMu.
type reservations map[int64]reservation // by site id

type reservation struct {
	owner int64
	bytes int64
}

// ownerBytes returns the bytes reserved for owner's sites other than except.
func (r reservations) ownerBytes(owner, except int64) int64 {
	var n int64
	for id, v := range r {
		if id != except && owner != 0 && v.owner == owner {
			n += v.bytes
		}
	}
	return n
}
