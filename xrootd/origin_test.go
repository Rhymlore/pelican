//go:build !windows

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

package xrootd

import (
	"bytes"
	"context"
	"crypto/elliptic"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/daemon"
	"github.com/pelicanplatform/pelican/origin_ui"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/server_utils"
	"github.com/pelicanplatform/pelican/test_utils"
	"github.com/pelicanplatform/pelican/utils"
	"golang.org/x/sync/errgroup"

	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/spf13/viper"
	"github.com/stretchr/testify/require"
)

func originMockup(ctx context.Context, egrp *errgroup.Group, t *testing.T) context.CancelFunc {
	originServer := &origin_ui.OriginServer{}

	// Create our own temp directory (for some reason t.TempDir() does not play well with xrootd)
	tmpPathPattern := "XRootD-Test_Origin*"
	tmpPath, err := os.MkdirTemp("", tmpPathPattern)
	require.NoError(t, err)

	// Need to set permissions or the xrootd process we spawn won't be able to write PID/UID files
	permissions := os.FileMode(0755)
	err = os.Chmod(tmpPath, permissions)
	require.NoError(t, err)

	viper.Set("ConfigDir", tmpPath)
	viper.Set("Xrootd.RunLocation", filepath.Join(tmpPath, "xrootd"))
	t.Cleanup(func() {
		os.RemoveAll(tmpPath)
	})

	// Increase the log level; otherwise, its difficult to debug failures
	viper.Set("Logging.Level", "Debug")
	config.InitConfig()
	err = config.InitServer(ctx, config.OriginType)
	require.NoError(t, err)

	err = config.GeneratePrivateKey(param.Server_TLSKey.GetString(), elliptic.P256())
	require.NoError(t, err)
	err = config.GenerateCert()
	require.NoError(t, err)

	err = CheckXrootdEnv(originServer)
	require.NoError(t, err)

	shutdownCtx, shutdownCancel := context.WithCancel(context.Background())

	err = SetUpMonitoring(shutdownCtx, egrp)
	require.NoError(t, err)

	configPath, err := ConfigXrootd(shutdownCtx, true)
	require.NoError(t, err)

	launchers, err := ConfigureLaunchers(false, configPath, false, false)
	require.NoError(t, err)

	err = daemon.LaunchDaemons(shutdownCtx, launchers, egrp)
	require.NoError(t, err)

	return shutdownCancel
}

func TestOrigin(t *testing.T) {
	ctx, cancel, egrp := test_utils.TestContext(context.Background(), t)
	defer func() { require.NoError(t, egrp.Wait()) }()
	defer cancel()

	viper.Reset()

	viper.Set("Origin.ExportVolume", t.TempDir()+":/test")
	viper.Set("Origin.Mode", "posix")
	// Disable functionality we're not using (and is difficult to make work on Mac)
	viper.Set("Origin.EnableCmsd", false)
	viper.Set("Origin.EnableMacaroons", false)
	viper.Set("Origin.EnableVoms", false)
	viper.Set("TLSSkipVerify", true)

	mockupCancel := originMockup(ctx, egrp, t)
	defer mockupCancel()

	// In this case a 403 means its running
	err := server_utils.WaitUntilWorking(ctx, "GET", param.Origin_Url.GetString(), "xrootd", 403)
	if err != nil {
		t.Fatalf("Unsuccessful test: Server encountered an error: %v", err)
	}
	fileTests := utils.TestFileTransferImpl{}
	ok, err := fileTests.RunTests(ctx, param.Origin_Url.GetString(), param.Origin_Url.GetString(), utils.OriginSelfFileTest)
	require.NoError(t, err)
	require.True(t, ok)

	viper.Reset()
}

func TestS3OriginConfig(t *testing.T) {
	ctx, cancel, egrp := test_utils.TestContext(context.Background(), t)
	defer func() { require.NoError(t, egrp.Wait()) }()
	defer cancel()

	viper.Reset()
	tmpDir := t.TempDir()

	// We need to start up a minio server, which is how we emulate S3. Open to better ways to do this!
	minIOServerCmd := exec.CommandContext(ctx, "minio", "server", "--quiet", tmpDir)
	minIOServerCmd.Env = []string{fmt.Sprintf("HOME=%s", tmpDir)}

	// Create a few buffers to grab outputs
	var outb, errb bytes.Buffer
	minIOServerCmd.Stdout = &outb
	minIOServerCmd.Stderr = &errb

	err := minIOServerCmd.Start()
	require.NoError(t, err)
	// Check for any other errors in the outputs
	if strings.Contains(strings.ToLower(outb.String()), "error") {
		t.Fatalf("Could not start the MinIO server: %s", outb.String())
	} else if errb.String() != "" {
		t.Fatalf("Could not start the MinIO server: %s", errb.String())
	}

	// Check if MinIO is running (by default at localhost:9000)
	endpoint := "localhost:9000"
	// Expect a 403 from this endpoint -- that means it's running
	err = server_utils.WaitUntilWorking(ctx, "GET", fmt.Sprintf("http://%s", endpoint), "xrootd", 403)
	if err != nil {
		t.Fatalf("Unsuccessful test: Server encountered an error: %v", err)
	}

	defer func() {
		err = minIOServerCmd.Process.Kill()
		require.NoError(t, err)
	}()

	// Let's create an unauthenticated bucket. First we set up a client instance
	// By default, the endpoint will require access/secret key with these values. Note that this doesn't
	// necessarily mean the bucket we create needs those keys, as the bucket will have its own IAM
	accessKey := "minioadmin"
	secretKey := "minioadmin"
	useSSL := false

	minioClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: useSSL,
	})
	require.NoError(t, err)

	// Create a new unauthenticated bucket. Under the hood, this will access the minio server endpoint
	// and do a PUT
	bucketName := "test-bucket"
	regionName := "test-region"
	serviceName := "test-name"
	err = minioClient.MakeBucket(context.Background(), bucketName, minio.MakeBucketOptions{})
	require.NoError(t, err)

	// Set bucket policy for public read access
	policy := `{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Principal":"*","Action":["s3:GetObject"],"Resource":["arn:aws:s3:::` + bucketName + `/*"]}]}`
	err = minioClient.SetBucketPolicy(context.Background(), bucketName, policy)
	require.NoError(t, err)

	// Upload a test file to the bucket
	testFilePath := filepath.Join(tmpDir, "test-file.txt")
	content := []byte("This is the content of the test file.")
	err = os.WriteFile(testFilePath, content, 0644)
	require.NoError(t, err)

	objectName := "test-file.txt"
	contentType := "application/octet-stream"
	_, err = minioClient.FPutObject(context.Background(), bucketName, objectName, testFilePath, minio.PutObjectOptions{ContentType: contentType})
	require.NoError(t, err)

	// All the setup to create the S3 server, add a bucket with a publicly-readable object is done. Now onto Pelican stuff
	// Set up the origin and try to pull a file
	viper.Set("Origin.Mode", "s3")
	viper.Set("Origin.S3Region", regionName)
	viper.Set("Origin.S3Bucket", bucketName)
	viper.Set("Origin.S3ServiceName", serviceName)
	viper.Set("Origin.S3ServiceUrl", fmt.Sprintf("http://%s", endpoint))
	// Disable functionality we're not using (and is difficult to make work on Mac)
	viper.Set("Origin.EnableCmsd", false)
	viper.Set("Origin.EnableMacaroons", false)
	viper.Set("Origin.EnableVoms", false)
	viper.Set("Origin.SelfTest", false)
	viper.Set("TLSSkipVerify", true)

	mockupCancel := originMockup(ctx, egrp, t)
	defer mockupCancel()

	// FOR NOW, we consider the test a success if the origin's xrootd server boots.
	// TODO: When we've made it easier to come back and test whole pieces of this process by disentangling our
	// *serve* commands, come back and make this e2e. The reason I'm punting is that in S3 mode, we also need all
	// of the web_ui stuff initialized to export the public signing keys (as we can't export via XRootD) and we
	// need a real token. These become much easier when we have an internally workable set of commands to do so.

	originEndpoint := fmt.Sprintf("%s/%s/%s/%s/%s", param.Origin_Url.GetString(), serviceName, regionName, bucketName, objectName)
	// Until we sort out the things we mentioned above, we should expect a 403 because we don't try to pass tokens
	// and we don't actually export any keys for token validation.
	err = server_utils.WaitUntilWorking(ctx, "GET", originEndpoint, "xrootd", 403)
	if err != nil {
		t.Fatalf("Unsucessful test: Server encountered an error: %v", err)
	}

	// One other quick check to do is that the namespace was correctly parsed:
	require.Equal(t, fmt.Sprintf("/%s/%s/%s", serviceName, regionName, bucketName), param.Origin_NamespacePrefix.GetString())
	viper.Reset()
}
