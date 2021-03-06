// Copyright 2016 e-Xpert Solutions SA. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package f5 provides a client for using the F5 API.
package f5

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
)

// Paths for file upload.
const (
	PathUploadImage = "/mgmt/cm/autodeploy/software-image-uploads"
	PathUploadFile  = "/mgmt/shared/file-transfer/uploads"

	// For backward compatibility
	UploadRESTPath = PathUploadFile
)

type UploadResponse struct {
	RemainingByteCount int64          `json:"remainingByteCount"`
	UsedChunks         map[string]int `json:"usedChunks"`
	TotalByteCount     int64          `json:"totalByteCount"`
	LocalFilePath      string         `json:"localFilePath"`
	TemporaryFilePath  string         `json:"temporaryFilePath"`
	Generation         int64          `json:"generation"`
	LastUpdateMicros   int64          `json:"lastUpdateMicros"`
}

// An authFunc is function responsible for setting necessary headers to
// perform authenticated requests.
type authFunc func(req *http.Request)

// A Client manages communication with the F5 API.
type Client struct {
	c        http.Client
	baseURL  string
	makeAuth authFunc

	// Transaction
	txID string // transaction ID to send for every request
}

// NewBasicClient creates a new F5 client with HTTP Basic Authentication.
//
// baseURL is the base URL of the F5 API server.
func NewBasicClient(baseURL, user, password string) (*Client, error) {
	return &Client{
		c:       http.Client{},
		baseURL: baseURL,
		makeAuth: authFunc(func(req *http.Request) {
			req.SetBasicAuth(user, password)
		}),
	}, nil
}

// NewTokenClient creates a new F5 client with token based authentication.
//
// baseURL is the base URL of the F5 API server.
func NewTokenClient(baseURL, user, password, loginProvName string, sslCheck bool) (*Client, error) {
	c := Client{c: http.Client{}, baseURL: baseURL}

	// Negociate token with a pair of username/password.
	data, err := json.Marshal(map[string]string{"username": user, "password": password, "loginProviderName": loginProvName})
	if err != nil {
		return nil, fmt.Errorf("failed to create token client (cannot marshal user credentials): %v", err)
	}

	req, err := http.NewRequest("POST", c.makeURL("/mgmt/shared/authn/login"), bytes.NewBuffer(data))
	if err != nil {
		return nil, fmt.Errorf("failed to create token client, (cannot create login request): %v", err)
	}
	req.Header.Add("Content-Type", "application/json")
	req.SetBasicAuth(user, password)

	client := http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: sslCheck},
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to create token client (token negociation failed): %v", err)
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("failed to create token client (token negociation failed): http status %s", resp.Status)
	}
	defer resp.Body.Close()

	tok := struct {
		Token struct {
			Token string `json:"token"`
		} `json:"token"`
	}{}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&tok); err != nil {
		return nil, fmt.Errorf("failed to create token client (cannot decode token): %v", err)
	}

	// Create auth function for token based authentication.
	c.makeAuth = authFunc(func(req *http.Request) {
		req.Header.Add("X-F5-Auth-Token", tok.Token.Token)
	})

	return &c, nil
}

// DisableCertCheck disables certificate verification, meaning that insecure
// certificate will not cause any error.
func (c *Client) DisableCertCheck() {
	c.c.Transport = &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
}

// Begin starts a transaction.
func (c *Client) Begin() (*Client, error) {
	// Send HTTP request to the API responsible for creating a new transaction.
	resp, err := c.SendRequest("POST", "/mgmt/tm/transaction", map[string]interface{}{})
	if err != nil {
		return nil, errors.New("cannot create request for starting a new transaction: " + err.Error())
	}
	defer resp.Body.Close()

	// Decode and validate transaction creation response.
	var tx struct {
		TransID int64  `json:"transId"`
		State   string `json:"state"`
	}
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&tx); err != nil {
		return nil, errors.New("cannot read transaction creation response: " + err.Error())
	}
	if tx.State != "STARTED" {
		return nil, fmt.Errorf("invalid transaction sate %q; want %q", tx.State, "STARTED")
	}

	// Create a new client from the current one, but with a transaction ID.
	newClient := c.clone()
	newClient.txID = strconv.FormatInt(tx.TransID, 10)

	return newClient, nil
}

// TransactionID returns the ID of the current transaction. If there is no
// active transaction, an empty string is returned.
func (c *Client) TransactionID() string {
	return c.txID
}

// Commit commits the transaction.
func (c *Client) Commit() error {
	if c.txID == "" {
		return errors.New("no transaction started")
	}

	txID := c.txID
	c.txID = ""

	data := map[string]interface{}{"state": "VALIDATING"}
	resp, err := c.SendRequest("PATCH", "/mgmt/tm/transaction/"+txID, data)
	if err != nil {
		return errors.New("cannot commit transaction: " + err.Error())
	}
	defer resp.Body.Close()

	return nil
}

// MakeRequest creates a request with headers appropriately set to make
// authenticated requests. This method must be called for every new request.
func (c *Client) MakeRequest(method, restPath string, data interface{}) (*http.Request, error) {
	var (
		req *http.Request
		err error
	)
	if data != nil {
		bs, err := json.Marshal(data)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal data into json: %v", err)
		}
		req, err = http.NewRequest(method, c.makeURL(restPath), bytes.NewBuffer(bs))
	} else {
		req, err = http.NewRequest(method, c.makeURL(restPath), nil)
	}
	if err != nil {
		return nil, fmt.Errorf("failed to create F5 authenticated request: %v", err)
	}
	req.Header.Add("Accept", "application/json")
	if c.txID != "" {
		req.Header.Add("X-F5-REST-Coordination-Id", c.txID)
	}
	c.makeAuth(req)
	return req, nil
}

func (c *Client) UploadFile(r io.Reader, filename string, filesize int64) (*UploadResponse, error) {
	req, err := c.MakeUploadRequest(PathUploadFile+"/"+filename, r, filesize)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if err := c.ReadError(resp); err != nil {
		return nil, err
	}

	var uploadResp UploadResponse
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(&uploadResp); err != nil {
		return nil, err
	}

	return &uploadResp, nil
}

func (c *Client) MakeUploadRequest(restPath string, r io.Reader, filesize int64) (*http.Request, error) {
	if filesize > 512*1024 {
		return nil, fmt.Errorf("file larger than %d are not yet supported", 512*1024)
	}
	req, err := http.NewRequest("POST", c.makeURL(restPath), r)
	if err != nil {
		return nil, fmt.Errorf("failed to create F5 authenticated request: %v", err)
	}
	req.Header.Add("Accept", "application/json")
	req.Header.Set("Content-Range", fmt.Sprintf("%d-%d/%d", 0, filesize-1, filesize))
	c.makeAuth(req)
	return req, nil
}

// Do sends an HTTP request and returns an HTTP response. It is just a wrapper
// arround http.Client Do method.
//
// Callers should close resp.Body when done reading from it.
//
// See http package documentation for more information:
//    https://golang.org/pkg/net/http/#Client.Do
func (c *Client) Do(req *http.Request) (*http.Response, error) {
	return c.c.Do(req)
}

// SendRequest is a shortcut for MakeRequest() + Do() + ReadError().
func (c *Client) SendRequest(method, restPath string, data interface{}) (*http.Response, error) {
	req, err := c.MakeRequest(method, restPath, data)
	if err != nil {
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	if err := c.ReadError(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp, nil
}

// ReadQuery performs a GET query and unmarshal the response (from JSON) into outputData.
//
// outputData must be a pointer.
func (c *Client) ReadQuery(restPath string, outputData interface{}) error {
	resp, err := c.SendRequest("GET", restPath, nil)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	if err := dec.Decode(outputData); err != nil {
		return err
	}
	return nil
}

// ModQuery performs a modification query such as POST, PUT or DELETE.
func (c *Client) ModQuery(method, restPath string, inputData interface{}) error {
	if method != "POST" && method != "PUT" && method != "DELETE" {
		return errors.New("invalid method " + method)
	}
	resp, err := c.SendRequest(method, restPath, inputData)
	if err != nil {
		return err
	}
	resp.Body.Close()
	return nil
}

// ReadError checks if a HTTP response contains an error and returns it.
func (c *Client) ReadError(resp *http.Response) error {
	if resp.StatusCode >= 400 {
		if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "application/json") {
			return errors.New("http response error: " + resp.Status)
		}
		errResp, err := NewRequestError(resp.Body)
		if err != nil {
			return err
		}
		return errResp
	}
	return nil
}

// makeURL creates an URL from the client base URL and a given REST path. What
// this function actually does is to concatenate the base URL and the REST path
// by handling trailing slashes.
func (c *Client) makeURL(restPath string) string {
	return strings.TrimSuffix(c.baseURL, "/") + "/" + strings.TrimPrefix(restPath, "/")
}

func (c *Client) clone() *Client {
	return &Client{
		c:        c.c,
		baseURL:  c.baseURL,
		makeAuth: c.makeAuth,
	}
}
