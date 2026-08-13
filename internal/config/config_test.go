package config

import "testing"

func TestFromEnvReadsGROBIDURL(t *testing.T) {
	t.Setenv("HERALD_GROBID_URL", "http://grobid.internal:8070")
	settings := FromEnv()
	if settings.GROBIDURL != "http://grobid.internal:8070" {
		t.Fatalf("unexpected GROBID URL: %q", settings.GROBIDURL)
	}
}
