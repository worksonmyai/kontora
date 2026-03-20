package summarizer

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCache_GetSet(t *testing.T) {
	c := NewCache(time.Minute)
	c.Set("key1", "summary text", "live", nil)

	entry, ok := c.Get("key1")
	require.True(t, ok)
	assert.Equal(t, "summary text", entry.Summary)
	assert.Equal(t, "live", entry.Source)
	assert.NoError(t, entry.Error)
}

func TestCache_Miss(t *testing.T) {
	c := NewCache(time.Minute)
	_, ok := c.Get("nonexistent")
	assert.False(t, ok)
}

func TestCache_Expiry(t *testing.T) {
	c := NewCache(10 * time.Millisecond)
	c.Set("key1", "summary", "live", nil)

	time.Sleep(20 * time.Millisecond)

	_, ok := c.Get("key1")
	assert.False(t, ok)
}

func TestCache_Delete(t *testing.T) {
	c := NewCache(time.Minute)
	c.Set("key1", "summary", "live", nil)

	c.Delete("key1")

	_, ok := c.Get("key1")
	assert.False(t, ok)
}

func TestCache_ErrorEntries(t *testing.T) {
	c := NewCache(time.Minute)
	testErr := errors.New("summarizer failed")
	c.Set("key1", "", "live", testErr)

	entry, ok := c.Get("key1")
	require.True(t, ok)
	assert.Empty(t, entry.Summary)
	assert.Equal(t, testErr, entry.Error)
}

func TestCache_CompositeKeys(t *testing.T) {
	c := NewCache(time.Minute)
	c.Set("t-001", "live summary", "live", nil)
	c.Set("t-001:code", "log summary", "logs", nil)

	live, ok := c.Get("t-001")
	require.True(t, ok)
	assert.Equal(t, "live summary", live.Summary)

	log, ok := c.Get("t-001:code")
	require.True(t, ok)
	assert.Equal(t, "log summary", log.Summary)
}
