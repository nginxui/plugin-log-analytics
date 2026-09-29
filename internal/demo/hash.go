package demo

import "hash/fnv"

// seed derives a stable 64-bit value from a domain and a key.
//
// Every fabricated value must be a pure function of its input. A shared
// math/rand source would make output depend on goroutine scheduling: the log
// parser runs a worker pool and memoises geo lookups per IP, so whichever
// answer landed first would be frozen in, differently on every run.
func seed(domain, key string) uint64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte("nginxui-demo|"))
	_, _ = h.Write([]byte(domain))
	_, _ = h.Write([]byte{'|'})
	_, _ = h.Write([]byte(key))
	return h.Sum64()
}

// pick chooses an element by hashed key. Returns the zero value for an empty
// table so callers do not have to guard.
func pick[T any](table []T, s uint64) T {
	var zero T
	if len(table) == 0 {
		return zero
	}
	return table[s%uint64(len(table))]
}
