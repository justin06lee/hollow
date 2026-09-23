package secrets

import (
	"strings"
	"testing"
)

func TestParseCSV(t *testing.T) {
	for name, c := range map[string]struct {
		csv   string
		names string
		sites string
	}{
		"chrome": {
			"name,url,username,password,note\n" +
				"github.com,https://github.com/login,octo,pw1,\n" +
				"github.com,https://github.com/,work,pw2,\n" +
				"Wi-Fi,,,pw3,\n" +
				"empty,https://x.io,,,\n",
			"github-octo,github-work,wi-fi", "github.com,github.com,",
		},
		"firefox": {
			"\"url\",\"username\",\"password\",\"httpRealm\",\"formActionOrigin\",\"guid\"\n" +
				"\"https://accounts.google.com\",\"me@gmail.com\",\"pw\",,\"\",\"{1}\"\n",
			"google", "accounts.google.com",
		},
		"bitwarden": {
			"folder,favorite,type,name,notes,fields,reprompt,login_uri,login_username,login_password,login_totp\n" +
				",,login,AWS Console,,,0,https://signin.aws.amazon.com,root,pw,otpauth://totp/aws?secret=GEZDGNBV\n" +
				",,note,Secret note,text,,0,,,,\n",
			"aws-console", "signin.aws.amazon.com",
		},
		"1password": {
			"\ufeffTitle,Url,Username,Password,OTPAuth,Favorite,Archived,Tags,Notes\n" +
				"Bank,www.bank.com,me,pw,,false,false,,\n",
			"bank", "bank.com",
		},
	} {
		logins, err := ParseCSV(strings.NewReader(c.csv))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		var sites []string
		for _, l := range logins {
			sites = append(sites, l.Site())
		}
		if got := strings.Join(Names(logins), ","); got != c.names {
			t.Errorf("%s: names %s, want %s", name, got, c.names)
		}
		if got := strings.Join(sites, ","); got != c.sites {
			t.Errorf("%s: sites %s, want %s", name, got, c.sites)
		}
	}
}
