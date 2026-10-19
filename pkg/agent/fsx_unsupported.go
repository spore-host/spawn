//go:build !linux

package agent

import "errors"

// errLustreUnsupported is what sysMountLustre returns off Linux, where spored
// never mounts FSx Lustre (#194): the mount fails and the caller reports it.
//
// It is a variable rather than an error built in each stub because on darwin
// and Windows the stub is the only sysMountLustre, and staticcheck read the
// caller's err != nil as constant there (SA4023), which kept `make check` from
// passing on a Mac (#1060). The comparison is not constant: Linux returns nil
// on success. Naming the error keeps staticcheck running in full on every GOOS
// instead of narrowing it to Linux.
var errLustreUnsupported = errors.New("FSx Lustre mounting is only supported on Linux")
