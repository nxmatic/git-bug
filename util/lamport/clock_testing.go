package lamport

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func testClock(t *testing.T, c Clock) {
	assert.Equal(t, Time(1), c.Time())

	val, err := c.Increment()
	assert.NoError(t, err)
	assert.Equal(t, Time(1), val)
	assert.Equal(t, Time(2), c.Time())

	err = c.Witness(41)
	assert.NoError(t, err)
	assert.Equal(t, Time(42), c.Time())

	err = c.Witness(41)
	assert.NoError(t, err)
	assert.Equal(t, Time(42), c.Time())

	err = c.Witness(30)
	assert.NoError(t, err)
	assert.Equal(t, Time(42), c.Time())
}
