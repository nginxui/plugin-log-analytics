package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLimit(t *testing.T) {
	limit, ok := Limit(1000<<20, true, 8000<<20, true, false)
	assert.True(t, ok)
	assert.Equal(t, int64(800<<20), limit, "a cgroup limit wins over the system memory")

	_, ok = Limit(1000<<20, true, 8000<<20, true, true)
	assert.False(t, ok, "an explicit GOMEMLIMIT wins")

	limit, ok = Limit(0, false, 2000<<20, true, false)
	assert.True(t, ok)
	assert.Equal(t, int64(1000<<20), limit, "half of the system memory without a cgroup limit")

	_, ok = Limit(0, false, 0, false, false)
	assert.False(t, ok, "no known memory, no soft limit")

	limit, ok = Limit(-1, true, 2000<<20, true, false)
	assert.True(t, ok)
	assert.Equal(t, int64(1000<<20), limit, "an invalid cgroup limit falls back to the system memory")

	_, ok = Limit(-1, true, 0, true, false)
	assert.False(t, ok)
}
