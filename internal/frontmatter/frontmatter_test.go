package frontmatter

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSplitBasic(t *testing.T) {
	input := "---\nid: test\nstatus: open\n---\n# Title\n\nBody text.\n"
	yaml, body, err := Split(input)
	require.NoError(t, err)
	assert.Equal(t, "id: test\nstatus: open", yaml)
	assert.Equal(t, "# Title\n\nBody text.\n", body)
}

func TestSplitNoFrontmatter(t *testing.T) {
	_, _, err := Split("# Just a heading\n")
	require.Error(t, err)
}

func TestSplitEmptyBody(t *testing.T) {
	input := "---\nid: test\n---\n"
	yaml, body, err := Split(input)
	require.NoError(t, err)
	assert.Equal(t, "id: test", yaml)
	assert.Equal(t, "", body)
}

func TestSplitDashesInBody(t *testing.T) {
	input := "---\nid: test\n---\n# Title\n\n---\n\nMore text.\n"
	yaml, body, err := Split(input)
	require.NoError(t, err)
	assert.Equal(t, "id: test", yaml)
	assert.Equal(t, "# Title\n\n---\n\nMore text.\n", body)
}

func TestSplitCRLF(t *testing.T) {
	input := "---\r\nid: test\r\nstatus: open\r\n---\r\n# Title\r\n"
	yaml, body, err := Split(input)
	require.NoError(t, err)
	assert.Equal(t, "id: test\nstatus: open", yaml)
	assert.Equal(t, "# Title\r\n", body)
}

func TestSplitMixedLineEndings(t *testing.T) {
	for _, header := range []string{"---\nid: test\n---\n", "---\r\nid: test\r\n---\r\n", "---\nid: test\r\n---\r\n"} {
		for _, tail := range []string{"# Title\r\n\r\n## Notes\r\nraw\r\n", "# Title\n\r\n## Notes\r\nraw\n---\nkeep"} {
			yaml, body, err := Split(header + tail)
			require.NoError(t, err)
			assert.Equal(t, "id: test", yaml)
			assert.Equal(t, tail, body)
		}
	}
}

func TestSplitNoClosing(t *testing.T) {
	_, _, err := Split("---\nid: test\nno closing delimiter\n")
	require.Error(t, err)
}

func TestSplitClosingAtEOF(t *testing.T) {
	input := "---\nid: test\n---"
	yaml, body, err := Split(input)
	require.NoError(t, err)
	assert.Equal(t, "id: test", yaml)
	assert.Equal(t, "", body)
}
