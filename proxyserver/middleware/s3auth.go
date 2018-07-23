//  Copyright (c) 2018 Rackspace
//
//  Licensed under the Apache License, Version 2.0 (the "License");
//  you may not use this file except in compliance with the License.
//  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or
//  implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.

package middleware

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/troubling/hummingbird/common/conf"
	"github.com/uber-go/tally"
)

var S3Subresources = map[string]bool{
	"acl":                          true,
	"delete":                       true,
	"lifecycle":                    true,
	"location":                     true,
	"logging":                      true,
	"notification":                 true,
	"partNumber":                   true,
	"policy":                       true,
	"requestPayment":               true,
	"torrent":                      true,
	"uploads":                      true,
	"uploadId":                     true,
	"versionId":                    true,
	"versioning":                   true,
	"versions":                     true,
	"website":                      true,
	"response-cache-control":       true,
	"response-content-disposition": true,
	"response-content-encoding":    true,
	"response-content-language":    true,
	"response-content-type":        true,
	"response-expires":             true,
	"cors":                         true,
	"tagging":                      true,
	"restore":                      true,
}

type S3AuthInfo struct {
	Key          string
	Signature    string
	StringToSign string
	Account      string
}

func (s *S3AuthInfo) validateSignature(secret []byte) bool {
	// S3 Auth signature V2 Validation
	mac := hmac.New(sha1.New, secret)
	mac.Write([]byte(s.StringToSign))
	sig1 := mac.Sum(nil)
	sig2, err := base64.StdEncoding.DecodeString(s.Signature)
	if err != nil {
		return false
	}
	// TODO: Add support for constat time compare
	return hmac.Equal(sig1, sig2)
}

type s3AuthHandler struct {
	next           http.Handler
	ctx            *ProxyContext
	requestsMetric tally.Counter
}

func (s *s3AuthHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	ctx := GetProxyContext(request)
	// Check if this is an S3 request
	var key, signature string
	authStr := request.Header.Get("Authorization")
	if authStr == "" {
		authStr = request.Form.Get("AWSAccessKeyId")
	}
	if authStr != "" {
		authStr = strings.TrimPrefix(authStr, "AWS ")
		i := strings.LastIndex(authStr, ":")
		if i < 0 {
			ctx.Authorize = func(r *http.Request) (bool, int) {
				return false, http.StatusForbidden
			}
			s.next.ServeHTTP(writer, request)
			return
		}
		key = authStr[0:i]
		signature = authStr[i+1:]
	}
	if authStr == "" {
		// Check params for auth info
		key = request.FormValue("AWSAccessKeyId")
		signature = request.FormValue("Signature")
	}
	if key == "" || signature == "" || ctx.S3Auth != nil {
		// Not an S3 request or already processed
		s.next.ServeHTTP(writer, request)
		return
	}

	// Wrap the writer so that we can capture errors and send correct S3 style responses
	writer = newS3ResponseWriterWrapper(writer, request)

	// TODO: Handle parameter style auth
	// TODO: Handle V2 signature validation
	// Setup the string to be signed
	var buf bytes.Buffer
	buf.WriteString(request.Method)
	buf.WriteString("\n")
	buf.WriteString(request.Header.Get("Content-MD5"))
	buf.WriteString("\n")
	buf.WriteString(request.Header.Get("Content-Type"))
	buf.WriteString("\n")
	if request.Header.Get("x-amz-date") != "" {
		buf.WriteString("\n")
	} else {
		buf.WriteString(request.Header.Get("Date"))
		buf.WriteString("\n")
	}
	akeys := make([]string, 0)
	for k := range request.Header {
		if strings.HasPrefix(strings.ToLower(k), "x-amz-") {
			akeys = append(akeys, k)
		}
	}
	// the headers need to be in sorted order before signing
	sort.Strings(akeys)
	for _, k := range akeys {
		for _, v := range request.Header[k] {
			buf.WriteString(fmt.Sprintf("%s:%s", strings.ToLower(k), v))
			buf.WriteString("\n")
		}
	}
	// NOTE: The following is for V2 Auth

	buf.WriteString(request.URL.Path)
	if request.URL.RawQuery != "" {
		queryParts := strings.Split(request.URL.RawQuery, "&")
		var signableQueryParts []string
		for _, v := range queryParts {
			if S3Subresources[v] {
				signableQueryParts = append(signableQueryParts, v)
			}
		}
		sort.Strings(signableQueryParts)
		ctx.Logger.Info(fmt.Sprintf("queryParts: %+v", queryParts))
		ctx.Logger.Info(fmt.Sprintf("signableQueryParts: %+v", signableQueryParts))
		if len(signableQueryParts) > 0 {
			buf.WriteString("?" + strings.Join(signableQueryParts, "&"))
		}
	}
	ctx.Logger.Debug(fmt.Sprintf("%v", buf.String()))
	ctx.Logger.Info(fmt.Sprintf("%v", buf.String()))
	ctx.S3Auth = &S3AuthInfo{
		StringToSign: buf.String(),
		Key:          key,
		Signature:    signature,
	}

	// TODO: Handle V4 signature validation

	s.next.ServeHTTP(writer, request)
}

func NewS3Auth(config conf.Section, metricsScope tally.Scope) (func(http.Handler) http.Handler, error) {
	enabled, ok := config.Section["enabled"]
	if !ok || strings.Compare(strings.ToLower(enabled), "false") == 0 {
		// s3api is disabled, so pass the request on
		return func(next http.Handler) http.Handler {
			return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				next.ServeHTTP(writer, request)
			})
		}, nil
	}
	RegisterInfo("s3Auth", map[string]interface{}{})
	return s3Auth(metricsScope.Counter("s3Auth_requests")), nil
}

func s3Auth(requestsMetric tally.Counter) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			(&s3AuthHandler{next: next, requestsMetric: requestsMetric}).ServeHTTP(writer, request)
		})
	}
}
