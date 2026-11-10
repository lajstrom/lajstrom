package probe

import (
	"net/http"
	"net/url"
)

// ans is an answer of status with the headers of kv (name, value, …) and body.
func ans(status int, body string, kv ...string) answer {
	h := http.Header{}
	for i := 0; i+1 < len(kv); i += 2 {
		h.Add(kv[i], kv[i+1])
	}
	u, _ := url.Parse("http://registry.test/v2/x/blobs/uploads/1")
	return answer{status: status, header: h, body: []byte(body), size: int64(len(body)), url: u}
}

// errAns is an answer of status with an error document of code.
func errAns(status int, code string) answer {
	return ans(status, `{"errors":[{"code":"`+code+`","message":"m"}]}`, "Content-Type", "application/json")
}

// plain404 is the plain-text 404 of a route that does not exist.
var plain404 = ans(http.StatusNotFound, "404 page not found\n", "Content-Type", "text/plain; charset=utf-8")
