package auth

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"kiro-go/config"
)

// The account carries three region fields: region (authentication), authRegion
// (token refresh endpoints) and apiRegion (Kiro data plane). RefreshToken used to
// read account.Region directly, so an account whose refresh endpoint lives in a
// different region than its data plane -- the whole reason authRegion exists --
// silently refreshed against the wrong oidc host. An account with authRegion set
// and region empty fell all the way back to us-east-1.
//
// EffectiveAuthRegion resolves the documented chain instead:
// account.authRegion > account.region > global authRegion > global region > us-east-1.
func TestRefreshTokenUsesEffectiveAuthRegion(t *testing.T) {
	cases := []struct {
		name       string
		account    config.Account
		wantRegion string
	}{
		{
			// The field exists precisely for this: refresh in one region, data
			// plane in another. Reading account.Region ignored it.
			name: "authRegion wins over region",
			account: config.Account{
				Region:     "ap-southeast-2",
				AuthRegion: "eu-west-1",
			},
			wantRegion: "eu-west-1",
		},
		{
			// Nothing else to fall back on, so this used to reach the us-east-1
			// default and refresh against the wrong host entirely.
			name:       "authRegion alone",
			account:    config.Account{AuthRegion: "eu-central-1"},
			wantRegion: "eu-central-1",
		},
		{
			// Unchanged behaviour for accounts that only set region.
			name:       "region when authRegion is empty",
			account:    config.Account{Region: "ap-southeast-2"},
			wantRegion: "ap-southeast-2",
		},
		{
			name:       "us-east-1 when neither is set",
			account:    config.Account{},
			wantRegion: "us-east-1",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotRegion := ""
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"accessToken":"at","refreshToken":"rt","expiresIn":3600}`))
			}))
			defer server.Close()

			oldURL := GetOIDCTokenURLForTest()
			SetOIDCTokenURLForTest(func(region string) string {
				gotRegion = region
				return server.URL + "/token"
			})
			defer SetOIDCTokenURLForTest(oldURL)

			// Proxy=nil keeps http.ProxyFromEnvironment from caching env state for
			// the proxy tests in this package.
			oldClient := SetGlobalAuthClientForTest(&http.Client{Transport: &http.Transport{}})
			defer SetGlobalAuthClientForTest(oldClient)

			acct := tc.account
			acct.ClientID = "cid"
			acct.ClientSecret = "secret"
			acct.RefreshToken = "old-refresh"

			if _, _, _, _, err := RefreshToken(&acct); err != nil {
				t.Fatalf("RefreshToken: %v", err)
			}
			if gotRegion != tc.wantRegion {
				t.Fatalf("refresh endpoint region = %q, want %q", gotRegion, tc.wantRegion)
			}
		})
	}
}
