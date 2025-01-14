// Copyright 2017 The Chromium Authors. All rights reserved.
// Use of this source code is governed by a BSD-style license that can be
// found in the LICENSE file.

package webpagereplay

import (
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

var (
	tmpdir    string
	nocleanup = flag.Bool("nocleanup", false, "If true, don't cleanup temp files on shutdown.")
)

func TestMain(m *testing.M) {
	flag.Parse()
	var err error
	tmpdir, err = ioutil.TempDir("", "webpagereplay_proxy_test")
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot make tempdir: %v", err)
		os.Exit(1)
	}
	ret := m.Run()
	if !*nocleanup {
		os.RemoveAll(tmpdir)
	}
	os.Exit(ret)
}

func TestEndToEnd(t *testing.T) {
	archiveFile := filepath.Join(tmpdir, "TestEndToEnd.json")

	// We will record responses from this server.
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Path {
		case "/img":
			w.Header().Set("Cache-Control", "public, max-age=3600")
			w.Header().Set("Content-Type", "image/webp")
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "fake image body")
		case "/206":
			w.Header().Set("Content-Type", "text/plain")
			w.Header().Set("Content-Length", "4")
			w.WriteHeader(http.StatusPartialContent)
			fmt.Fprint(w, "body")
		case "/post":
			w.Header().Set("Cache-Control", "private")
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusOK)
			io.Copy(w, req.Body)
		default:
			w.WriteHeader(http.StatusOK)
			fmt.Fprint(w, "default response")
		}
	}))
	defer origin.Close()

	// Start a proxy for the origin server that will construct an archive file.
	recordArchive, err := OpenWritableArchive(archiveFile)
	if err != nil {
		t.Fatalf("OpenWritableArchive: %v", err)
	}
	var transformers []ResponseTransformer
	recordServer := httptest.NewServer(NewRecordingProxy(recordArchive, "http", transformers))
	recordTransport := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse(recordServer.URL)
		},
	}

	// Send a bunch of URLs to the server and record the responses.
	urls := []string{
		origin.URL + "/img",
		origin.URL + "/206",
		origin.URL + "/post",
	}
	type RecordedResponse struct {
		Code   int
		Header http.Header
		Body   string
	}
	recordResponse := func(u string, tr *http.Transport) (*RecordedResponse, error) {
		var req *http.Request
		var err error
		if strings.HasSuffix(u, "/post") {
			req, err = http.NewRequest("POST", u, strings.NewReader("this is the POST body"))
		} else {
			req, err = http.NewRequest("GET", u, nil)
		}
		if err != nil {
			return nil, fmt.Errorf("NewRequest(%s): %v", u, err)
		}
		resp, err := tr.RoundTrip(req)
		if err != nil {
			return nil, fmt.Errorf("RoundTrip(%s): %v", u, err)
		}
		defer resp.Body.Close()
		body, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			return nil, fmt.Errorf("ReadBody(%s): %v", u, err)
		}
		return &RecordedResponse{resp.StatusCode, resp.Header, string(body)}, nil
	}
	recorded := make(map[string]*RecordedResponse)
	for _, u := range urls {
		resp, err := recordResponse(u, recordTransport)
		if err != nil {
			t.Fatal(err)
		}
		recorded[u] = resp
	}

	// Shutdown and flush the archive.
	recordServer.Close()
	if err := recordArchive.Close(); err != nil {
		t.Fatalf("CloseArchive: %v", err)
	}
	recordArchive = nil
	recordServer = nil
	recordTransport = nil

	// Open a replay server using the saved archive.
	replayArchive, err := OpenArchive(archiveFile)
	if err != nil {
		t.Fatalf("OpenArchive: %v", err)
	}
	replayServer := httptest.NewServer(NewReplayingProxy(replayArchive, "http", transformers))
	replayTransport := &http.Transport{
		Proxy: func(*http.Request) (*url.URL, error) {
			return url.Parse(replayServer.URL)
		},
	}

	// Re-send the same URLs and ensure we get the same response.
	for _, u := range urls {
		resp, err := recordResponse(u, replayTransport)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := resp, recorded[u]; !reflect.DeepEqual(got, want) {
			t.Errorf("response doesn't match for %v:\n%+v\n%+v", u, got, want)
		}
	}
	// Check that a URL not found in the archive returns 404.
	resp, err := recordResponse(origin.URL+"/not_found_in_archive", replayTransport)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := resp.Code, http.StatusNotFound; got != want {
		t.Errorf("status code for /not_found_in_archive: got: %v want: %v", got, want)
	}
}
