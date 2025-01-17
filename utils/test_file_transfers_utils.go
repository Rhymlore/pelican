/***************************************************************
 *
 * Copyright (C) 2023, Pelican Project, Morgridge Institute for Research
 *
 * Licensed under the Apache License, Version 2.0 (the "License"); you
 * may not use this file except in compliance with the License.  You may
 * obtain a copy of the License at
 *
 *    http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 ***************************************************************/

// This is a utility file that provides a TestFileTransferImpl struct with a `RunTests` function
// to allow any Pelican server to issue a file transfer test to a XRootD server

package utils

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/url"
	"time"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pkg/errors"
)

type (
	TestType         string
	TestFileTransfer interface {
		generateFileTestScitoken(audienceUrl string) (string, error)
		UploadTestfile(ctx context.Context, baseUrl string, testType TestType) (string, error)
		DownloadTestfile(ctx context.Context, downloadUrl string) error
		DeleteTestfile(ctx context.Context, fileUrl string) error
		RunTests(ctx context.Context, baseUrl string, testType TestType) (bool, error)
	}
	TestFileTransferImpl struct {
		audienceUrl string
		issuerUrl   string
		testType    TestType
		testBody    string
	}
)

const (
	OriginSelfFileTest TestType = "self-test"
	DirectorFileTest   TestType = "director-test"
)

const (
	selfTestBody     string = "This object was created by the Pelican self-test functionality"
	directorTestBody string = "This object was created by the Pelican director-test functionality"
)

func (t TestType) String() string {
	return string(t)
}

// TODO: Replace by CreateEncodedToken once it's free from main package #320
func (t TestFileTransferImpl) generateFileTestScitoken() (string, error) {
	// Issuer is whichever server that initiates the test, so it's the server itself
	issuerUrl := param.Server_ExternalWebUrl.GetString()
	if t.issuerUrl != "" { // Get from param if it's not empty
		issuerUrl = t.issuerUrl
	}
	if issuerUrl == "" { // if both are empty, then error
		return "", errors.New("Failed to create token: Invalid iss, Server_ExternalWebUrl is empty")
	}

	fTestTokenCfg := TokenConfig{
		TokenProfile: WLCG,
		Lifetime:     time.Minute,
		Issuer:       issuerUrl,
		Audience:     []string{t.audienceUrl},
		Version:      "1.0",
		Subject:      "origin",
		Claims:       map[string]string{"scope": "storage.read:/ storage.modify:/"},
	}

	// CreateToken also handles validation for us
	tok, err := fTestTokenCfg.CreateToken()
	if err != nil {
		return "", errors.Wrap(err, "failed to create file test token")
	}

	return tok, nil
}

// Private function to upload a test file to the `baseUrl` of an exported xrootd file direcotry
// the test file content is based on the `testType` attribute
func (t TestFileTransferImpl) uploadTestfile(ctx context.Context, baseUrl string) (string, error) {
	tkn, err := t.generateFileTestScitoken()
	if err != nil {
		return "", errors.Wrap(err, "Failed to create a token for test file transfer")
	}

	uploadURL, err := url.Parse(baseUrl)
	if err != nil {
		return "", errors.Wrap(err, "The baseUrl is not parseable as a URL")
	}
	uploadURL.Path = "/pelican/monitoring/" + t.testType.String() + "-" + time.Now().Format(time.RFC3339) + ".txt"

	req, err := http.NewRequestWithContext(ctx, "PUT", uploadURL.String(), bytes.NewBuffer([]byte(t.testBody)))
	if err != nil {
		return "", errors.Wrap(err, "Failed to create POST request for monitoring upload")
	}

	req.Header.Set("Authorization", "Bearer "+tkn)

	client := http.Client{Transport: config.GetTransport()}

	resp, err := client.Do(req)
	if err != nil {
		return "", errors.Wrap(err, "Failed to start request for test file upload")
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		return "", errors.Errorf("Error response %v from test file upload: %v", resp.StatusCode, resp.Status)
	}

	return uploadURL.String(), nil
}

// Private function to download a file from downloadUrl and make sure it matches the test file
// content based on the `testBody` attribute
func (t TestFileTransferImpl) downloadTestfile(ctx context.Context, downloadUrl string) error {
	tkn, err := t.generateFileTestScitoken()
	if err != nil {
		return errors.Wrap(err, "Failed to create a token for test file transfer download")
	}

	req, err := http.NewRequestWithContext(ctx, "GET", downloadUrl, nil)
	if err != nil {
		return errors.Wrap(err, "Failed to create GET request for test file transfer download")
	}
	req.Header.Set("Authorization", "Bearer "+tkn)

	client := http.Client{Transport: config.GetTransport()}

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "Failed to start request for test file transfer download")
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return errors.Wrap(err, "Failed to get response body from test file transfer download")
	}
	if string(body) != t.testBody {
		return errors.Errorf("Contents of test file transfer body do not match upload: %v", body)
	}

	if resp.StatusCode > 299 {
		return errors.Errorf("Error response %v from test file transfer download: %v", resp.StatusCode, resp.Status)
	}

	return nil
}

// Private function to delete a test file from `fileUrl`
func (t TestFileTransferImpl) deleteTestfile(ctx context.Context, fileUrl string) error {
	tkn, err := t.generateFileTestScitoken()
	if err != nil {
		return errors.Wrap(err, "Failed to create a token for the test file transfer deletion")
	}

	req, err := http.NewRequestWithContext(ctx, "DELETE", fileUrl, nil)
	if err != nil {
		return errors.Wrap(err, "Failed to create DELETE request for test file transfer deletion")
	}
	req.Header.Set("Authorization", "Bearer "+tkn)

	client := http.Client{Transport: config.GetTransport()}

	resp, err := client.Do(req)
	if err != nil {
		return errors.Wrap(err, "Failed to start request for test file transfer deletion")
	}
	defer resp.Body.Close()

	if resp.StatusCode > 299 {
		return errors.Errorf("Error response %v from test file transfer deletion: %v", resp.StatusCode, resp.Status)
	}

	return nil
}

// Run a file transfer test suite with upload/download/delete a test file from
// the server and a xrootd service. It expects `baseUrl` to be the url to the xrootd
// endpoint, `issuerUrl` be the url to issue scitoken for file transfer, and the
// test file content/name be based on `testType`
//
// Note that for this test to work, you need to have the `issuerUrl` registered in
// your xrootd as a list of trusted token issuers and the issuer is expected to follow
// WLCG rules for issuer metadata discovery and public key access
//
// Read more: https://github.com/WLCG-AuthZ-WG/common-jwt-profile/blob/master/profile.md#token-verification
func (t TestFileTransferImpl) RunTests(ctx context.Context, baseUrl, issuerUrl string, testType TestType) (bool, error) {
	t.audienceUrl = baseUrl
	t.issuerUrl = issuerUrl
	t.testType = testType
	if testType == OriginSelfFileTest {
		t.testBody = selfTestBody
	} else if testType == DirectorFileTest {
		t.testBody = directorTestBody
	} else {
		return false, errors.New("Unsupported testType: " + testType.String())
	}

	downloadUrl, err := t.uploadTestfile(ctx, baseUrl)
	if err != nil {
		return false, errors.Wrap(err, "Test file transfer failed during upload")
	}
	err = t.downloadTestfile(ctx, downloadUrl)
	if err != nil {
		return false, errors.Wrap(err, "Test file transfer failed during download")
	}
	err = t.deleteTestfile(ctx, downloadUrl)
	if err != nil {
		return false, errors.Wrap(err, "Test file transfer failed during delete")
	}
	return true, nil
}
