package server

import "time"

// pendingPerIP caps the sign-in attempts one client address can have in flight. Starting
// an attempt needs no login, so without a cap one client could fill the map and keep every
// real user from signing in.
const pendingPerIP = 20

// pendingEntry is what makePendingRoom needs to know about a login attempt.
type pendingEntry interface {
	owner() string // the client address that started it
	deadline() time.Time
}

// makePendingRoom drops expired attempts from m and then makes room for one more from ip:
// when ip already has pendingPerIP attempts, its oldest is dropped, and when m is still at
// max entries, the oldest of all is dropped. A new attempt is therefore never refused, and a
// client can only push out its own attempts (apart from the global cap, which one address
// cannot reach alone). The caller holds the lock guarding m.
func makePendingRoom[V pendingEntry](m map[string]V, ip string, max int, now time.Time) {
	var (
		mine                      int
		oldestMine, oldestAny     string
		oldestMineAt, oldestAnyAt time.Time
	)
	for k, p := range m {
		if now.After(p.deadline()) {
			delete(m, k)
			continue
		}
		if oldestAny == "" || p.deadline().Before(oldestAnyAt) {
			oldestAny, oldestAnyAt = k, p.deadline()
		}
		if p.owner() == ip {
			mine++
			if oldestMine == "" || p.deadline().Before(oldestMineAt) {
				oldestMine, oldestMineAt = k, p.deadline()
			}
		}
	}
	if mine >= pendingPerIP {
		delete(m, oldestMine)
		if oldestMine == oldestAny {
			oldestAny = ""
		}
	}
	if len(m) >= max && oldestAny != "" {
		delete(m, oldestAny)
	}
}
