package proxy

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestAccessLog(t *testing.T) {
	dir := t.TempDir()
	lines := `{"StartUTC":"2026-07-25T10:00:00Z","RouterName":"app-abcd1234-0@file","RequestMethod":"GET","RequestPath":"/x","OriginStatus":200,"Duration":2500000,"ClientHost":"1.2.3.4"}
{"StartUTC":"2026-07-25T10:00:01Z","RouterName":"app-ffff0000-0@file","RequestMethod":"GET","RequestPath":"/other","OriginStatus":200,"Duration":1000000,"ClientHost":"1.2.3.4"}
{"StartUTC":"2026-07-25T10:00:02Z","RouterName":"app-abcd1234-0@file","RequestMethod":"POST","RequestPath":"/y","OriginStatus":500,"Duration":9000000,"ClientHost":"5.6.7.8"}
not json
`
	require.NoError(t, os.WriteFile(filepath.Join(dir, "access.log"), []byte(lines), 0o644))
	p := &Proxy{dir: dir}
	got := p.AccessLog("abcd1234-0000-0000-0000-000000000000", 10)
	require.Len(t, got, 2)
	// newest first
	assert.Equal(t, "POST", got[0].Method, "bad first entry: %+v", got[0])
	assert.Equal(t, 500, got[0].Status, "bad first entry: %+v", got[0])
	assert.Equal(t, int64(9), got[0].MS, "bad first entry: %+v", got[0])
	assert.Equal(t, "/x", got[1].Path, "bad second entry: %+v", got[1])
	assert.Equal(t, "1.2.3.4", got[1].Client, "bad second entry: %+v", got[1])
}
