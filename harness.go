package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/mainplane-ai/mainplane/pkg/statefile"
)

// sessionInfo is a session as the harness describes it. The list leaves
// Name, Created and ContextUsed zero.
type sessionInfo struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Status      statefile.Status `json:"status"`
	Config      statefile.Conf   `json:"config"`
	N           int              `json:"n"`
	ContextUsed int              `json:"context_used"`
	Created     time.Time        `json:"created"`
}

// logPaths are where the harness copied a session's records when it
// stopped, which its system prompt says a continuation names: the log on each
// connected worker of its config. None when the workers cannot be listed.
func (b *bridge) logPaths(id string, conf statefile.Conf) []string {
	var ws []struct{ Name, Scratch, Refused string }
	if err := b.call("GET", "/workers", "", nil, &ws); err != nil {
		log.Printf("workers: %v", err)
		return nil
	}
	scratch := map[string]string{}
	for _, w := range ws {
		if w.Refused == "" && w.Scratch != "" {
			scratch[w.Name] = w.Scratch
		}
	}
	return logLines(id, conf.Workers, scratch)
}

// logLines names <scratch>/logs/<id>.log on each of workers that has a scratch.
func logLines(id string, workers []statefile.Worker, scratch map[string]string) []string {
	var out []string
	for _, w := range workers {
		if s, ok := scratch[w.Name]; ok {
			out = append(out, s+"/logs/"+id+".log on "+w.Name)
		}
	}
	return out
}

func (b *bridge) create(conf statefile.Conf, via string) (string, error) {
	body, err := json.Marshal(conf)
	if err != nil {
		return "", err
	}
	var out struct{ ID string }
	err = b.call("POST", "/sessions?via="+via, "application/json", bytes.NewReader(body), &out)
	return out.ID, err
}

// records reads the session's records after record after, long polling for
// wait when there are none yet ("" returns at once).
func (b *bridge) records(id string, after int, wait string) ([]statefile.Record, error) {
	u := fmt.Sprintf("%s/sessions/%s/records?after=%d", b.harness, id, after)
	if wait != "" {
		u += "&wait=" + wait
	}
	req, _ := http.NewRequest("GET", u, nil)
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 && resp.StatusCode < 500 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
		return nil, &refusedError{resp.Status + ": " + strings.TrimSpace(string(msg))}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s", resp.Status)
	}
	var recs []statefile.Record
	br := bufio.NewReader(resp.Body)
	for {
		r, err := statefile.Decode(br)
		if err == io.EOF {
			return recs, nil
		}
		if err != nil {
			return recs, err
		}
		recs = append(recs, r)
	}
}

// refusedError is a 4xx from the harness, which a retry does not mend.
type refusedError struct{ msg string }

func (e *refusedError) Error() string { return e.msg }

// gone is whether the session no longer exists, as after a delete, rather
// than the call being refused for another reason, such as a revoked key. The
// harness answers every error with 400, so only its text tells.
func (e *refusedError) gone() bool { return strings.Contains(e.msg, "no such file") }

func (b *bridge) info(id string) (sessionInfo, error) {
	var i sessionInfo
	return i, b.call("GET", "/sessions/"+id, "", nil, &i)
}

func (b *bridge) call(method, path, ctype string, body io.Reader, out any) error {
	req, err := http.NewRequestWithContext(context.Background(), method, b.harness+path, body)
	if err != nil {
		return err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return b.do(req, out)
}

func (b *bridge) do(req *http.Request, out any) error {
	req.Header.Set("Authorization", "Bearer "+b.key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	if out == nil || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}
