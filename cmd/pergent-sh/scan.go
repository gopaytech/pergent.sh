package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

	"github.com/gopaytech/pergent.sh/internal/readiness"
)

// scan-repo runs in the "scan" container of a readiness scan Job
// (kube.BuildScanJob), after the "clone" container cloned the default branch
// into --dir and wrote the commit to --commit-file, or its failure to
// --error-file. It scans the clone, which is untrusted (readiness.ScanTree
// follows no symlink), with the input in --input-file and posts the result to
// --result-url with the scan token (PERGENT_SH_SCAN_TOKEN) as bearer. The
// input is a file, not the environment: it can be larger than one environment
// string may be. The command holds no other secret.

// scanOptions are the flags of scan-repo.
type scanOptions struct {
	dir, commitFile, errorFile, inputFile, resultURL string
}

const (
	msgScanCloneUnknown = msgCloneUnknown
	msgScanInput        = "the scan input couldn't be read"
	msgScanRead         = "couldn't scan the files in the clone; see the log of the Job's scan container"
	msgScanTooLarge     = "the scan result is too large to send"
)

// parseScanArgs reads the flags of "scan-repo". --result-url is an http or
// https URL.
func parseScanArgs(args []string, stderr io.Writer) (scanOptions, error) {
	flags := flag.NewFlagSet("scan-repo", flag.ContinueOnError)
	flags.SetOutput(stderr)
	var o scanOptions
	flags.StringVar(&o.dir, "dir", "", "the clone")
	flags.StringVar(&o.commitFile, "commit-file", "", "the file the clone wrote its commit to")
	flags.StringVar(&o.errorFile, "error-file", "", "the file the clone wrote its failure to")
	flags.StringVar(&o.inputFile, "input-file", "", "the file that holds the scan input (JSON)")
	flags.StringVar(&o.resultURL, "result-url", "", "where to post the result")
	if err := flags.Parse(args); err != nil {
		return scanOptions{}, err
	}
	switch {
	case flags.NArg() > 0:
		return scanOptions{}, fmt.Errorf("unexpected argument %q\n%s", flags.Arg(0), usage)
	case o.dir == "" || o.commitFile == "" || o.errorFile == "" || o.inputFile == "" || o.resultURL == "":
		return scanOptions{}, errors.New("scan-repo needs --dir, --commit-file, --error-file, --input-file and --result-url\n" + usage)
	}
	if err := checkResultURL(o.resultURL); err != nil {
		return scanOptions{}, err
	}
	return o, nil
}

// runScanRepo is the scan-repo command: it reads the token from the
// environment and posts the result. It exits 0 when pergent.sh took the
// result and 1 when not.
func runScanRepo(ctx context.Context, o scanOptions, stderr io.Writer) int {
	token := os.Getenv("PERGENT_SH_SCAN_TOKEN")
	if token == "" {
		fmt.Fprintln(stderr, "environment variable PERGENT_SH_SCAN_TOKEN is not set or empty")
		return 1
	}
	if err := scanRepo(ctx, o, token, newResultClient(), stderr); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}

// scanRepo scans the clone and posts what it found. A clone that failed, an
// input that can't be read, a tree that can't be scanned and a result over
// readiness.MaxResultBytes are each posted as an error: only a result
// pergent.sh did not take is an error here.
func scanRepo(ctx context.Context, o scanOptions, token string, client *http.Client, stderr io.Writer) error {
	res := scanResult(ctx, o, stderr)
	body, err := scanResultBody(res)
	if err != nil {
		return err
	}
	if err := postResult(ctx, client, o.resultURL, token, body, stderr); err != nil {
		return err
	}
	if res.Error != "" {
		fmt.Fprintln(stderr, "sent the error:", res.Error)
	} else {
		fmt.Fprintf(stderr, "sent the scan of %d files at %s\n", res.Files, res.Commit)
	}
	return nil
}

// scanResultBody is res as JSON, or an error result when it is over
// readiness.MaxResultBytes.
func scanResultBody(res readiness.JobResult) ([]byte, error) {
	body, err := json.Marshal(res)
	if err != nil {
		return nil, fmt.Errorf("encode the result: %w", err)
	}
	if len(body) > readiness.MaxResultBytes {
		body, err = json.Marshal(readiness.JobResult{Version: readiness.ResultVersion, Error: msgScanTooLarge})
		if err != nil {
			return nil, fmt.Errorf("encode the result: %w", err)
		}
	}
	return body, nil
}

// scanResult is the result to post: the clone's error, or the scan of the
// tree. The tree is not read when the clone failed.
func scanResult(ctx context.Context, o scanOptions, stderr io.Writer) readiness.JobResult {
	failed := func(text string) readiness.JobResult {
		return readiness.JobResult{Version: readiness.ResultVersion, Error: text}
	}
	text, ok, err := readSmall(o.errorFile, maxErrorFile)
	if err != nil {
		fmt.Fprintln(stderr, "reading the error file:", err)
		return failed(msgScanCloneUnknown)
	}
	if ok {
		if text = strings.TrimSpace(text); text == "" {
			text = msgScanCloneUnknown
		}
		return failed(text)
	}
	commit, ok, err := readSmall(o.commitFile, maxCommitFile)
	if err != nil || !ok {
		if err != nil {
			fmt.Fprintln(stderr, "reading the commit file:", err)
		}
		return failed(msgScanCloneUnknown)
	}
	in, err := readScanInput(o.inputFile)
	if err != nil {
		fmt.Fprintln(stderr, "reading the scan input:", err)
		return failed(msgScanInput)
	}
	res, err := readiness.ScanTree(ctx, o.dir, in)
	if err != nil {
		fmt.Fprintln(stderr, "scanning the clone:", err)
		return failed(msgScanRead)
	}
	res.Version = readiness.ResultVersion
	res.Commit = strings.TrimSpace(commit)
	return res
}

// readScanInput reads the JobInput in the file: JSON of at most
// readiness.MaxInputBytes, with no unknown field and nothing after it.
func readScanInput(file string) (readiness.JobInput, error) {
	text, ok, err := readSmall(file, readiness.MaxInputBytes+1)
	switch {
	case err != nil:
		return readiness.JobInput{}, err
	case !ok:
		return readiness.JobInput{}, errors.New("no such file")
	case len(text) > readiness.MaxInputBytes:
		return readiness.JobInput{}, fmt.Errorf("the input is over %d bytes", readiness.MaxInputBytes)
	}
	var in readiness.JobInput
	dec := json.NewDecoder(bytes.NewReader([]byte(text)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		return readiness.JobInput{}, err
	}
	if dec.Decode(&struct{}{}) != io.EOF {
		return readiness.JobInput{}, errors.New("data after the input")
	}
	return in, nil
}
