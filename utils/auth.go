package utils

import (
	"encoding/base64"
	"errors"
	"github.com/kr/fernet"
	"net/http"
	"os"
	"strings"
	"time"
)

var (
	OneHundredYears = time.Hour * 24 * 365 * 100
	keys            = fernet.MustDecodeKeys(strings.Split(os.Getenv("SECRETS"), ":")...)
)

func ParseAuth(r *http.Request) (user, pass string, err error) {
	header, ok := r.Header["Authorization"]
	if !ok {
		err = errors.New("Authorization header not set.")
		return
	}

	auth := strings.SplitN(header[0], " ", 2)
	if len(auth) != 2 {
		err = errors.New("Malformed header.")
		return
	}

	userPass, err := base64.StdEncoding.DecodeString(auth[1])
	if err != nil {
		err = errors.New("Malformed encoding.")
		return
	}

	parts := strings.Split(string(userPass), ":")

	//In this case, the request requires db-backed authentication.
	//the token.id is expected to be in parts[1] (password field).
	if parts[0] == "l2met" && len(parts[1]) > 0 {
		user = parts[0]
		pass = parts[1]
		return
	}
	//If we have gotten here, we have a signed, db-less authentication reque
	//If we can verify and decrypt, then we will pass the decrypted credenti
	//to the caller. Most of the time, the username and password will be
	//credentials to outlet providers. (e.g. Librato creds or Graphite creds
	//We care about the validity of those credentials here. If they are wron
	//the metrics will be dropped at the outlet. Keep an eye on http
	//authentication errors from the log output of the outlets.
	if s := fernet.VerifyAndDecrypt(userPass, OneHundredYears, keys); s != nil {
		parts := strings.Split(string(s), ":")
		user = parts[0]
		pass = parts[1]
		return
	}

	//If the user is not == "l2met" then dbless-auth is requested.
	//ATM we assume the first part (user field) contains a base64 encoded
	//representation of the outlet credentials.
	if len(parts[0]) > 0 {
		var decodedPart []byte
		decodedPart, err = base64.StdEncoding.DecodeString(parts[0])
		outletCreds := strings.Split(string(decodedPart), ":")
		//If the : is absent in parts[0], outletCreds[0] will contain the entire string in parts[0].
		user = outletCreds[0]
		//It is not required for the outletCreds to contain a pass.
		//The empty string that is returned will be handled by the outlet.
		if len(outletCreds) > 1 {
			pass = outletCreds[1]
		}
	}
	return
}
