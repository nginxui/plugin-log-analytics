package memory

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestLimit(t *testing.T) {
	limit, ok := Limit(1000<<20, true, false)
	assert.True(t, ok)
	assert.Equal(t, int64(800<<20), limit)

	_, ok = Limit(1000<<20, true, true)
	assert.False(t, ok, "an explicit GOMEMLIMIT wins")

	_, ok = Limit(0, false, false)
	assert.False(t, ok, "no cgroup limit, no soft limit")

	_, ok = Limit(-1, true, false)
	assert.False(t, ok)
}
