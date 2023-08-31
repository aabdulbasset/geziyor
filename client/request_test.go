package client

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestMeta(t *testing.T) {
	req, err := NewRequest("GET", "https://github.com/aabdulbasset/geziyor", nil)
	assert.NoError(t, err)
	req.Meta["key"] = "value"

	assert.Equal(t, req.Meta["key"], "value")
}
