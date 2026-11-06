// addlink creates an app.bsky.actor.link record with a custom icon blob and
// appends a strong reference to that record to the account's profile.
//
// Usage:
//
//	./addlink <handle> <app-password> <https-url> <title> <image-file>
//
// Optional environment variables:
//
//	ATPROTO_PDS      Override automatic PDS discovery (e.g. https://northsky.social)
//	ATPROTO_RESOLVER Handle resolver; defaults to https://bsky.social
//
// This program uses only the Go standard library.
package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	linkCollection    = "app.bsky.actor.link"
	profileCollection = "app.bsky.actor.profile"
	maxIconBytes      = 100_000
)

type client struct {
	http    *http.Client
	pds     string
	token   string
	userDID string
}

type apiError struct {
	Error   string `json:"error"`
	Message string `json:"message"`
}

type sessionResponse struct {
	DID       string `json:"did"`
	AccessJWT string `json:"accessJwt"`
}

type didDocument struct {
	Service []struct {
		ID              string `json:"id"`
		Type            string `json:"type"`
		ServiceEndpoint string `json:"serviceEndpoint"`
	} `json:"service"`
}

type blobResponse struct {
	Blob json.RawMessage `json:"blob"`
}

type createRecordResponse struct {
	URI string `json:"uri"`
	CID string `json:"cid"`
}

type getRecordResponse struct {
	CID   string                 `json:"cid"`
	Value map[string]interface{} `json:"value"`
}

func main() {
	if err := run(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "addlink:", err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) != 5 {
		return errors.New("usage: ./addlink <handle> <app-password> <https-url> <title> <image-file>")
	}
	handle, password, destination, title, filename := args[0], args[1], args[2], args[3], args[4]
	if handle == "" || password == "" {
		return errors.New("handle and app password must not be empty")
	}

	destinationURL, err := url.Parse(destination)
	if err != nil || destinationURL.Scheme != "https" || destinationURL.Host == "" {
		return errors.New("URL must be a valid HTTPS URL")
	}
	if title == "" {
		return errors.New("title must not be empty (use a short label of at most 40 characters)")
	}
	// Runes are a conservative approximation of the lexicon's 40-grapheme
	// limit: this may reject some combined emoji/diacritic sequences early,
	// but will not knowingly send a title longer than 40 code points.
	if utf8.RuneCountInString(title) > 40 || len(title) > 320 {
		return errors.New("title is too long (maximum 40 characters / 320 UTF-8 bytes)")
	}

	imageBytes, contentType, err := readIcon(filename)
	if err != nil {
		return err
	}

	resolver := strings.TrimRight(os.Getenv("ATPROTO_RESOLVER"), "/")
	if resolver == "" {
		resolver = "https://bsky.social"
	}
	did, err := resolveHandle(resolver, handle)
	if err != nil {
		return err
	}
	pds := strings.TrimRight(os.Getenv("ATPROTO_PDS"), "/")
	if pds == "" {
		pds, err = discoverPDS(did)
		if err != nil {
			return err
		}
	}

	c := &client{
		http: &http.Client{Timeout: 60 * time.Second},
		pds:  pds,
	}
	fmt.Printf("Logging in to %s as %s...\n", pds, handle)
	if err := c.login(did, password); err != nil {
		return err
	}
	if c.userDID != did {
		return fmt.Errorf("the PDS authenticated a different DID (%s) than the handle resolved to (%s); check the handle, PDS, and resolver", c.userDID, did)
	}

	// Read the profile before creating anything so we can enforce the 10-link
	// limit and preserve every other existing profile field.
	profile, profileCID, profileExists, err := c.getProfile()
	if err != nil {
		return err
	}
	links, err := existingLinks(profile)
	if err != nil {
		return err
	}
	if len(links) >= 10 {
		return errors.New("this profile already has 10 links; remove one before adding another")
	}

	fmt.Printf("Uploading %s icon (%d bytes)...\n", contentType, len(imageBytes))
	blob, err := c.uploadBlob(imageBytes, contentType)
	if err != nil {
		return fmt.Errorf("upload icon: %w", err)
	}

	linkRecord := map[string]interface{}{
		"$type":     linkCollection,
		"url":       destination,
		"title":     title,
		"icon":      json.RawMessage(blob),
		"createdAt": time.Now().UTC().Format(time.RFC3339Nano),
	}
	// json.RawMessage nested in an interface is emitted as the blob object,
	// rather than as a base64-encoded byte slice.
	linkJSON, err := json.Marshal(linkRecord)
	if err != nil {
		return fmt.Errorf("encode link record: %w", err)
	}

	fmt.Println("Creating profile-link record...")
	created, err := c.createRecord(linkCollection, linkJSON)
	if err != nil {
		return fmt.Errorf("create link record: %w", err)
	}

	// Add the new reference to the end, preserving the user's current order.
	links = append(links, map[string]interface{}{"uri": created.URI, "cid": created.CID})
	profile["$type"] = profileCollection
	profile["links"] = links
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		_ = c.deleteRecord(linkCollection, recordKey(created.URI))
		return fmt.Errorf("encode updated profile: %w", err)
	}

	fmt.Println("Adding link to profile...")
	if err := c.putProfile(profileJSON, profileCID, profileExists); err != nil {
		// Best-effort cleanup so an unsuccessful profile update does not leave
		// an unused link record behind.
		cleanupErr := c.deleteRecord(linkCollection, recordKey(created.URI))
		if cleanupErr != nil {
			return fmt.Errorf("update profile: %w (also could not clean up the new link record: %v; record URI: %s)", err, cleanupErr, created.URI)
		}
		return fmt.Errorf("update profile: %w", err)
	}

	fmt.Println("Done!")
	fmt.Printf("Title: %s\nURL:   %s\nIcon:  %s\nRecord: %s\n", title, destination, filename, created.URI)
	return nil
}

func readIcon(filename string) ([]byte, string, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, "", fmt.Errorf("read icon file: %w", err)
	}
	if len(data) == 0 {
		return nil, "", errors.New("icon file is empty")
	}
	if len(data) > maxIconBytes {
		return nil, "", fmt.Errorf("icon is %d bytes; app.bsky.actor.link icons must be 100,000 bytes or smaller", len(data))
	}

	// Sniff the file bytes instead of trusting the filename extension.
	var contentType string
	switch {
	case bytes.HasPrefix(data, []byte{0x89, 'P', 'N', 'G', '\r', '\n', 0x1a, '\n'}):
		contentType = "image/png"
	case len(data) >= 3 && bytes.Equal(data[:3], []byte{0xff, 0xd8, 0xff}):
		contentType = "image/jpeg"
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		contentType = "image/webp"
	default:
		detected := http.DetectContentType(data)
		mediaType, _, _ := mime.ParseMediaType(detected)
		return nil, "", fmt.Errorf("unsupported icon format (%s); use PNG, JPEG, or WebP", mediaType)
	}
	return data, contentType, nil
}

func resolveHandle(resolver, handle string) (string, error) {
	query := url.Values{"handle": []string{handle}}
	discoveryClient := &http.Client{Timeout: 30 * time.Second}
	body, err := request(discoveryClient, http.MethodGet,
		resolver+"/xrpc/com.atproto.identity.resolveHandle?"+query.Encode(), nil, "", "")
	if err != nil {
		return "", fmt.Errorf("resolve handle %q using %s: %w (you can set ATPROTO_RESOLVER to another public resolver)", handle, resolver, err)
	}
	var resolved struct {
		DID string `json:"did"`
	}
	if err := json.Unmarshal(body, &resolved); err != nil || resolved.DID == "" {
		return "", fmt.Errorf("resolver returned an invalid DID for handle %q", handle)
	}
	return resolved.DID, nil
}

func discoverPDS(did string) (string, error) {
	didURL, err := didDocumentURL(did)
	if err != nil {
		return "", err
	}
	discoveryClient := &http.Client{Timeout: 30 * time.Second}
	docBody, err := request(discoveryClient, http.MethodGet, didURL, nil, "", "")
	if err != nil {
		return "", fmt.Errorf("fetch DID document %s: %w", did, err)
	}
	var doc didDocument
	if err := json.Unmarshal(docBody, &doc); err != nil {
		return "", fmt.Errorf("parse DID document for %s: %w", did, err)
	}
	for _, service := range doc.Service {
		if (strings.HasSuffix(service.ID, "#atproto_pds") || service.Type == "AtprotoPersonalDataServer") && service.ServiceEndpoint != "" {
			endpoint, err := url.Parse(service.ServiceEndpoint)
			if err != nil || endpoint.Scheme != "https" || endpoint.Host == "" {
				return "", fmt.Errorf("DID document for %s contains an invalid PDS endpoint", did)
			}
			return strings.TrimRight(service.ServiceEndpoint, "/"), nil
		}
	}
	return "", fmt.Errorf("DID document for %s does not declare an #atproto_pds service; set ATPROTO_PDS to your PDS URL", did)
}

func didDocumentURL(did string) (string, error) {
	switch {
	case strings.HasPrefix(did, "did:plc:"):
		return "https://plc.directory/" + url.PathEscape(did), nil
	case strings.HasPrefix(did, "did:web:"):
		parts := strings.Split(strings.TrimPrefix(did, "did:web:"), ":")
		if len(parts) == 0 || parts[0] == "" {
			return "", fmt.Errorf("invalid did:web identifier: %s", did)
		}
		host, err := url.PathUnescape(parts[0])
		if err != nil {
			return "", fmt.Errorf("invalid did:web host in %s: %w", did, err)
		}
		base := "https://" + host
		if len(parts) == 1 {
			return base + "/.well-known/did.json", nil
		}
		segments := make([]string, 0, len(parts)-1)
		for _, part := range parts[1:] {
			segment, err := url.PathUnescape(part)
			if err != nil {
				return "", fmt.Errorf("invalid did:web path in %s: %w", did, err)
			}
			segments = append(segments, url.PathEscape(segment))
		}
		return base + "/" + strings.Join(segments, "/") + "/did.json", nil
	default:
		return "", fmt.Errorf("unsupported DID method for %s (expected did:plc or did:web)", did)
	}
}

func (c *client) login(identifier, password string) error {
	payload, _ := json.Marshal(map[string]string{"identifier": identifier, "password": password})
	body, err := request(c.http, http.MethodPost, c.pds+"/xrpc/com.atproto.server.createSession", payload, "application/json", "")
	if err != nil {
		return fmt.Errorf("login failed: %w", err)
	}
	var session sessionResponse
	if err := json.Unmarshal(body, &session); err != nil {
		return fmt.Errorf("parse login response: %w", err)
	}
	if session.AccessJWT == "" || session.DID == "" {
		return errors.New("PDS login response did not include an access token and DID")
	}
	c.token = session.AccessJWT
	c.userDID = session.DID
	return nil
}

func (c *client) uploadBlob(data []byte, contentType string) (json.RawMessage, error) {
	body, err := request(c.http, http.MethodPost, c.pds+"/xrpc/com.atproto.repo.uploadBlob", data, contentType, c.token)
	if err != nil {
		return nil, err
	}
	var result blobResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if len(result.Blob) == 0 || string(result.Blob) == "null" {
		return nil, errors.New("PDS upload response did not contain a blob")
	}
	return result.Blob, nil
}

func (c *client) getProfile() (map[string]interface{}, string, bool, error) {
	query := url.Values{
		"repo":       []string{c.userDID},
		"collection": []string{profileCollection},
		"rkey":       []string{"self"},
	}
	endpoint := c.pds + "/xrpc/com.atproto.repo.getRecord?" + query.Encode()
	body, status, err := requestWithStatus(c.http, http.MethodGet, endpoint, nil, "", c.token)
	if err != nil {
		if status == http.StatusBadRequest || status == http.StatusNotFound {
			var apiErr apiError
			_ = json.Unmarshal(body, &apiErr)
			if apiErr.Error == "RecordNotFound" {
				return map[string]interface{}{"$type": profileCollection}, "", false, nil
			}
		}
		return nil, "", false, fmt.Errorf("read profile record: %w", err)
	}
	var result getRecordResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, "", false, fmt.Errorf("parse profile record: %w", err)
	}
	if result.Value == nil || result.CID == "" {
		return nil, "", false, errors.New("PDS returned an incomplete profile record")
	}
	return result.Value, result.CID, true, nil
}

func existingLinks(profile map[string]interface{}) ([]interface{}, error) {
	value, ok := profile["links"]
	if !ok || value == nil {
		return []interface{}{}, nil
	}
	links, ok := value.([]interface{})
	if !ok {
		return nil, errors.New("profile's links field is not an array; refusing to overwrite it")
	}
	return links, nil
}

func (c *client) createRecord(collection string, record []byte) (*createRecordResponse, error) {
	payload, err := json.Marshal(map[string]interface{}{
		"repo":       c.userDID,
		"collection": collection,
		"record":     json.RawMessage(record),
	})
	if err != nil {
		return nil, err
	}
	body, err := request(c.http, http.MethodPost, c.pds+"/xrpc/com.atproto.repo.createRecord", payload, "application/json", c.token)
	if err != nil {
		return nil, err
	}
	var result createRecordResponse
	if err := json.Unmarshal(body, &result); err != nil {
		return nil, err
	}
	if result.URI == "" || result.CID == "" {
		return nil, errors.New("createRecord response did not include URI and CID")
	}
	return &result, nil
}

func (c *client) putProfile(record []byte, currentCID string, exists bool) error {
	payload := map[string]interface{}{
		"repo":       c.userDID,
		"collection": profileCollection,
		"rkey":       "self",
		"record":     json.RawMessage(record),
	}
	if exists {
		payload["swapRecord"] = currentCID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = request(c.http, http.MethodPost, c.pds+"/xrpc/com.atproto.repo.putRecord", encoded, "application/json", c.token)
	return err
}

func (c *client) deleteRecord(collection, rkey string) error {
	payload, _ := json.Marshal(map[string]string{
		"repo":       c.userDID,
		"collection": collection,
		"rkey":       rkey,
	})
	_, err := request(c.http, http.MethodPost, c.pds+"/xrpc/com.atproto.repo.deleteRecord", payload, "application/json", c.token)
	return err
}

func recordKey(uri string) string {
	parts := strings.Split(uri, "/")
	if len(parts) == 0 {
		return ""
	}
	return parts[len(parts)-1]
}

func request(httpClient *http.Client, method, endpoint string, body []byte, contentType, token string) ([]byte, error) {
	response, _, err := requestWithStatus(httpClient, method, endpoint, body, contentType, token)
	if err != nil {
		return response, err
	}
	return response, nil
}

func requestWithStatus(httpClient *http.Client, method, endpoint string, body []byte, contentType, token string) ([]byte, int, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, endpoint, reader)
	if err != nil {
		return nil, 0, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if readErr != nil {
		return nil, resp.StatusCode, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var apiErr apiError
		_ = json.Unmarshal(responseBody, &apiErr)
		detail := apiErr.Message
		if detail == "" {
			detail = apiErr.Error
		}
		if detail == "" {
			detail = strings.TrimSpace(string(responseBody))
		}
		if detail == "" {
			detail = resp.Status
		}
		return responseBody, resp.StatusCode, fmt.Errorf("%s: %s", resp.Status, detail)
	}
	return responseBody, resp.StatusCode, nil
}
