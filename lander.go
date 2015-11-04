package lander

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
)

const (
	requestFn      = "request"
	responseFn     = "response"
	unmatchedReqFn = "unmatched_request"
)

// Path can be set to specify which subdirectory to use
var Path = "lander"

// Create is the main method for lander: it creates an httptest.Server matching
// received request to precomputed responses. It also takes as argument a
// function that is called on every request.
func Create(
	suffix string,
	munge func(*http.Request, *http.Response),
) (*httptest.Server, error) {
	responses, err := getResponses(suffix)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch responses: %v", err)
	}
	p := Path
	if suffix != "" {
		p = p + "-" + suffix
	}
	absPath, _ := filepath.Abs(p)

	ts := httptest.NewServer(
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			err = rewindRequest(r)
			if err != nil {
				panic(err)
			}

			for _, resp := range responses {
				actual := r.Body.(*body).String()
				expected := (resp.Request.Body).(*body).String()

				bodiesMatch := actual == expected

				// If body contains backticks, assume we want a regex match.
				regexpMatch := strings.Index(expected, "`") >= 0
				if regexpMatch {
					backtickRegex := regexp.MustCompile("`[^`]*`")
					// Find all sequences that we don't want to escape.
					matches := backtickRegex.FindAllString(expected, -1)
					// Replace what used to be in those sequences with an empty string.
					// A string like "abc`.+`def" then becomes "abc``def"
					withoutRegexes := backtickRegex.ReplaceAllLiteralString(expected, "``")
					// Quote all regex meta characters.
					withoutRegexesQuoted := regexp.QuoteMeta(withoutRegexes)
					// Substitute back in each unescaped sequences between the backticks.
					i := 0
					withRegexesQuoted := backtickRegex.ReplaceAllStringFunc(
						withoutRegexesQuoted,
						func(_ string) string {
							// The match includes the backticks, strip those out.
							match := matches[i][1 : len(matches[i])-1]
							i++
							return match
						},
					)
					bodiesMatch = regexp.MustCompile(withRegexesQuoted).Match([]byte(actual))
				}

				if r.Method == resp.Request.Method &&
					r.URL.String() == resp.Request.URL.String() &&
					bodiesMatch {

					munge(r, resp)
					for h, v := range resp.Header {
						w.Header().Add(h, v[0])
					}
					w.WriteHeader(resp.StatusCode)
					io.Copy(w, resp.Body)
					(resp.Body).(*body).Rewind()
					return
				}
			}

			reqBytes, err := dumpRequest(r, true)
			if err != nil {
				panic(fmt.Errorf("failed to dump umatched_request: %v", err))
			}
			err = ioutil.WriteFile(
				filepath.Join(absPath, unmatchedReqFn), reqBytes, 0666)
			if err != nil {
				panic(fmt.Errorf("error writing unmatched_request: %v", err))
			}
			panic(fmt.Errorf("umatched request dumped: %q", r.URL))
		}))

	return ts, nil
}

// getResponses retrieves the existing request responses pairs and construts
// responses objects. The responses objects have a reference to the associated
// request to which it should be matched.
func getResponses(
	suffix string,
) (map[string]*http.Response, error) {
	var dirs []string
	responses := map[string]*http.Response{}

	p := Path
	if suffix != "" {
		p = p + "-" + suffix
	}
	absPath, _ := filepath.Abs(p)

	fis, err := ioutil.ReadDir(absPath)
	if err != nil {
		return nil, err
	}

	for _, fi := range fis {
		if fi.Name() == unmatchedReqFn {
			continue
		}
		if !fi.IsDir() {
			return nil, fmt.Errorf(
				"unexpected non-dir file in lander path: %s", fi.Name())
		}
		dirs = append(dirs, filepath.Join(absPath, fi.Name()))
	}

	for _, dir := range dirs {
		inReq, err := os.Open(path.Join(dir, requestFn))
		if err != nil {
			continue
		}
		req, err := http.ReadRequest(bufio.NewReader(inReq))
		if err != nil {
			return nil, fmt.Errorf("error parsing request for %q: %v", dir, err)
		}
		err = rewindRequest(req)
		if err != nil {
			return nil, err
		}

		inRes, err := os.Open(path.Join(dir, responseFn))
		if err != nil {
			continue
		}
		res, err := http.ReadResponse(bufio.NewReader(inRes), req)
		if err != nil {
			return nil, fmt.Errorf("error parsing response for %q: %v", dir, err)
		}
		err = rewindResponse(res)
		if err != nil {
			return nil, err
		}

		responses[dir] = res
	}

	return responses, nil
}

const bodyLimit = 10 * 1024 * 1024 // 10MB, which is Go's default

// body is a type which implements the io.ReadCloser and io.Seeker interfaces,
// and is intended for use as a buffered request body with support for multiple
// consumptions.
type body struct {
	strings.Reader
	s string
}

func newBody(s string) *body {
	return &body{*strings.NewReader(s), s}
}

func rewindRequest(req *http.Request) error {
	var reqBuf bytes.Buffer
	_, err := io.CopyN(&reqBuf, req.Body, bodyLimit+1)
	req.Body.Close()

	if (err != nil && err != io.EOF) || reqBuf.Len() > bodyLimit {
		return fmt.Errorf("error rewinding request: %v", err)
	}
	req.Body = newBody(reqBuf.String())
	return nil
}

func rewindResponse(res *http.Response) error {
	var resBuf bytes.Buffer
	_, err := io.CopyN(&resBuf, res.Body, bodyLimit+1)
	res.Body.Close()

	if (err != nil && err != io.EOF) || resBuf.Len() > bodyLimit {
		return fmt.Errorf("error rewinding response: %v", err)
	}
	res.Body = newBody(resBuf.String())
	return nil
}

// Close does absolutely nothing, and is here only so that Body satisfies
// io.ReadCloser.
func (r *body) Close() error { return nil }

// Rewind seeks the request body to the very beginning. It is equivalent to
// calling Seek(0, 0), but contains fewer magic incants.
func (r *body) Rewind() error {
	_, err := r.Seek(0, 0)
	return err
}

// String returns the entire contents of the request body as a string.
func (r *body) String() string {
	return r.s
}

var _ io.ReadCloser = &body{}
var _ io.Seeker = &body{}

// The rest of these functions are vendored from httputil and modified to make
// dumpRequest dump the Content Lenght header

func drainBody(b io.ReadCloser) (r1, r2 io.ReadCloser, err error) {
	var buf bytes.Buffer
	if _, err = buf.ReadFrom(b); err != nil {
		return nil, nil, err
	}
	if err = b.Close(); err != nil {
		return nil, nil, err
	}
	return ioutil.NopCloser(&buf), ioutil.NopCloser(bytes.NewReader(buf.Bytes())), nil
}

var reqWriteExcludeHeaderDump = map[string]bool{
	"Host":              true, // not in Header map anyway
	"Transfer-Encoding": true,
	"Trailer":           true,
}

func valueOrDefault(value, def string) string {
	if value != "" {
		return value
	}
	return def
}

func dumpRequest(req *http.Request, body bool) (dump []byte, err error) {
	save := req.Body
	if !body || req.Body == nil {
		req.Body = nil
	} else {
		save, req.Body, err = drainBody(req.Body)
		if err != nil {
			return
		}
	}

	var b bytes.Buffer

	fmt.Fprintf(&b, "%s %s HTTP/%d.%d\r\n", valueOrDefault(req.Method, "GET"),
		req.URL.RequestURI(), req.ProtoMajor, req.ProtoMinor)

	host := req.Host
	if host == "" && req.URL != nil {
		host = req.URL.Host
	}
	if host != "" {
		fmt.Fprintf(&b, "Host: %s\r\n", host)
	}

	chunked := len(req.TransferEncoding) > 0 && req.TransferEncoding[0] == "chunked"
	if len(req.TransferEncoding) > 0 {
		fmt.Fprintf(&b, "Transfer-Encoding: %s\r\n", strings.Join(req.TransferEncoding, ","))
	}
	if req.Close {
		fmt.Fprintf(&b, "Connection: close\r\n")
	}

	err = req.Header.WriteSubset(&b, reqWriteExcludeHeaderDump)
	if err != nil {
		return
	}

	io.WriteString(&b, "\r\n")

	if req.Body != nil {
		var dest io.Writer = &b
		if chunked {
			dest = httputil.NewChunkedWriter(dest)
		}
		_, err = io.Copy(dest, req.Body)
		if chunked {
			dest.(io.Closer).Close()
			io.WriteString(&b, "\r\n")
		}
	}

	req.Body = save
	if err != nil {
		return
	}
	dump = b.Bytes()
	return
}
