//go:build unix

package main

import "syscall"

// setUmask makes new files and directories group-writable. Under OpenShift's restricted-v2
// SCC the container runs as a random UID in group 0, and a later run (another namespace, a
// restored volume) may get a different UID: group write is what lets it keep using the
// data volume. Others still get no write access.
func setUmask() { syscall.Umask(0o002) }
