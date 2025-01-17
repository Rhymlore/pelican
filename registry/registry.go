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

// Package registry handles namespace registration in Pelican ecosystem.
//
//   - It handles the logic to spin up a "registry" server for namespace management,
//     including a web UI for interactive namespace registration, approval, and browsing.
//   - It provides a CLI tool `./pelican namespace <command> <args>` to list, register, and delete a namespace
//
// To register a namespace, first spin up registry server by `./pelican registry serve -p <your-port-number>`, and then use either
// the CLI tool or go to registry web UI at `https://localhost:<your-port-number>/view/`, and follow instructions for next steps.
package registry

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	"github.com/lestrrat-go/jwx/v2/jwk"
	"github.com/lestrrat-go/jwx/v2/jwt"
	"github.com/pelicanplatform/pelican/config"
	"github.com/pelicanplatform/pelican/oauth2"
	"github.com/pelicanplatform/pelican/param"
	"github.com/pelicanplatform/pelican/token_scopes"
	"github.com/pkg/errors"
	log "github.com/sirupsen/logrus"

	// use this sqlite driver instead of the one from
	// github.com/mattn/go-sqlite3, because this one
	// doesn't require compilation with CGO_ENABLED
	_ "modernc.org/sqlite"
)

var OIDC struct {
	ClientID           string
	ClientSecret       string
	Scope              string
	DeviceAuthEndpoint string
	TokenEndpoint      string
	UserInfoEndpoint   string
	GrantType          string
}

var (
	// Loading of public/private keys for signing challenges
	serverCredsLoad    sync.Once
	serverCredsPrivKey *ecdsa.PrivateKey
	serverCredsErr     error
)

type Response struct {
	VerificationURLComplete string `json:"verification_uri_complete"`
	DeviceCode              string `json:"device_code"`
}

type TokenResponse struct {
	AccessToken string `json:"access_token"`
	Error       string `json:"error"`
}

type checkNamespaceExistsReq struct {
	Prefix string `json:"prefix"`
	PubKey string `json:"pubkey"`
}

type checkNamespaceExistsRes struct {
	PrefixExists bool   `json:"prefix_exists"`
	KeyMatch     bool   `json:"key_match"`
	Message      string `json:"message"`
	Error        string `json:"error"`
}

type checkStatusReq struct {
	Prefix string `json:"prefix"`
}

type checkStatusRes struct {
	Approved bool `json:"approved"`
}

// Various auxiliary functions used for client-server security handshakes
type NamespaceConfig struct {
	JwksUri string `json:"jwks_uri"`
}

/*
Various auxiliary functions used for client-server security handshakes
*/
type registrationData struct {
	ClientNonce     string `json:"client_nonce"`
	ClientPayload   string `json:"client_payload"`
	ClientSignature string `json:"client_signature"`

	ServerNonce     string `json:"server_nonce"`
	ServerPayload   string `json:"server_payload"`
	ServerSignature string `json:"server_signature"`

	Pubkey           json.RawMessage `json:"pubkey"`
	AccessToken      string          `json:"access_token"`
	Identity         string          `json:"identity"`
	IdentityRequired string          `json:"identity_required"`
	DeviceCode       string          `json:"device_code"`
	Prefix           string          `json:"prefix"`
}

func matchKeys(incomingKey jwk.Key, registeredNamespaces []string) (bool, error) {
	// If this is the case, we want to make sure that at least one of the superspaces has the
	// same registration key as the incoming. This guarantees the owner of the superspace is
	// permitting the action (assuming their keys haven't been stolen!)
	foundMatch := false
	for _, ns := range registeredNamespaces {
		keyset, _, err := getNamespaceJwksByPrefix(ns)
		if err != nil {
			return false, errors.Wrapf(err, "Cannot get keyset for %s from the database", ns)
		}

		// A super inelegant way to compare keys, but for whatever reason the keyset.Index(key) method
		// doesn't seem to actually recognize when a key is in the keyset, even if that key decodes to
		// the exact same JSON as a key in the set...
		for it := (keyset).Keys(context.Background()); it.Next(context.Background()); {
			pair := it.Pair()
			registeredKey := pair.Value.(jwk.Key)
			registeredKeyBuf, err := json.Marshal(registeredKey)
			if err != nil {
				return false, errors.Wrapf(err, "failed to marshal a key registered to %s into JSON", ns)
			}
			incomingKeyBuf, err := json.Marshal(incomingKey)
			if err != nil {
				return false, errors.Wrap(err, "failed to marshal the incoming key into JSON")
			}

			if string(registeredKeyBuf) == string(incomingKeyBuf) {
				foundMatch = true
				break
			}
		}
	}

	return foundMatch, nil
}

func keySignChallenge(ctx *gin.Context, data *registrationData, action string) error {
	if data.ClientNonce != "" && data.ClientPayload != "" && data.ClientSignature != "" &&
		data.ServerNonce != "" && data.ServerPayload != "" && data.ServerSignature != "" {
		err := keySignChallengeCommit(ctx, data, action)
		if err != nil {
			return errors.Wrap(err, "commit failed")
		}
	} else if data.ClientNonce != "" {
		err := keySignChallengeInit(ctx, data)
		if err != nil {
			return errors.Wrap(err, "init failed")
		}

	} else {
		ctx.JSON(http.StatusMultipleChoices, gin.H{"error": "MISSING PARAMETERS"})
		return errors.New("key sign challenge was missing parameters")
	}
	return nil
}

func generateNonce() (string, error) {
	nonce := make([]byte, 32)
	_, err := rand.Read(nonce)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(nonce), nil
}

func loadServerKeys() (*ecdsa.PrivateKey, error) {
	// Note: go 1.21 introduces `OnceValues` which automates this procedure.
	// TODO: Reimplement the function once we switch to a minimum of 1.21
	serverCredsLoad.Do(func() {
		issuerFileName := param.IssuerKey.GetString()
		serverCredsPrivKey, serverCredsErr = config.LoadPrivateKey(issuerFileName)
	})
	return serverCredsPrivKey, serverCredsErr
}

func signPayload(payload []byte, privateKey *ecdsa.PrivateKey) ([]byte, error) {
	hash := sha256.Sum256(payload)
	signature, err := privateKey.Sign(rand.Reader, hash[:], crypto.SHA256) // Use crypto.SHA256 instead of the hash[:]
	if err != nil {
		return nil, err
	}
	return signature, nil
}

func verifySignature(payload []byte, signature []byte, publicKey *ecdsa.PublicKey) bool {
	hash := sha256.Sum256(payload)
	return ecdsa.VerifyASN1(publicKey, hash[:], signature)
}

func keySignChallengeInit(ctx *gin.Context, data *registrationData) error {
	serverNonce, err := generateNonce()
	if err != nil {
		ctx.JSON(500, gin.H{"error": "Failed to generate nonce for key sign challenge"})
		return errors.Wrap(err, "Failed to generate nonce for key-sign challenge")
	}

	serverPayload := []byte(data.ClientNonce + data.ServerNonce)

	privateKey, err := loadServerKeys()
	if err != nil {
		ctx.JSON(500, gin.H{"error": "Server is unable to generate a key sign challenge"})
		return errors.Wrap(err, "Failed to load the server's private key")
	}

	serverSignature, err := signPayload(serverPayload, privateKey)
	if err != nil {
		ctx.JSON(500, gin.H{"error": "Failure when signing the challenge"})
		return errors.Wrap(err, "Failed to sign payload for key-sign challenge")
	}

	ctx.JSON(http.StatusOK, gin.H{
		"server_nonce":     serverNonce,
		"client_nonce":     data.ClientNonce,
		"server_payload":   hex.EncodeToString(serverPayload),
		"server_signature": hex.EncodeToString(serverSignature),
	})
	return nil
}

func keySignChallengeCommit(ctx *gin.Context, data *registrationData, action string) error {
	// Validate the client's jwks as a set here
	key, err := validateJwks(string(data.Pubkey))
	if err != nil {
		return err
	}

	var rawkey interface{} // This is the raw key, like *rsa.PrivateKey or *ecdsa.PrivateKey
	if err := key.Raw(&rawkey); err != nil {
		return errors.Wrap(err, "failed to generate raw pubkey from jwks")
	}

	clientPayload := []byte(data.ClientNonce + data.ServerNonce)
	clientSignature, err := hex.DecodeString(data.ClientSignature)
	if err != nil {
		ctx.JSON(500, gin.H{"error": "Failed to decode client's signature"})
		return errors.Wrap(err, "Failed to decode the client's signature")
	}
	clientVerified := verifySignature(clientPayload, clientSignature, (rawkey).(*ecdsa.PublicKey))
	serverPayload, err := hex.DecodeString(data.ServerPayload)
	if err != nil {
		ctx.JSON(500, gin.H{"error": "Failed to decode the server's payload"})
		return errors.Wrap(err, "Failed to decode the server's payload")
	}

	serverSignature, err := hex.DecodeString(data.ServerSignature)
	if err != nil {
		ctx.JSON(500, gin.H{"error": "Failed to decode the server's signature"})
		return errors.Wrap(err, "Failed to decode the server's signature")
	}

	serverPrivateKey, err := loadServerKeys()
	if err != nil {
		ctx.JSON(500, gin.H{"error": "Failed to load server's private key"})
		return errors.Wrap(err, "Failed to decode the server's private key")
	}
	serverPubkey := serverPrivateKey.PublicKey
	serverVerified := verifySignature(serverPayload, serverSignature, &serverPubkey)

	if clientVerified && serverVerified {
		if action == "register" {
			log.Debug("Registering namespace ", data.Prefix)

			// Check if prefix exists before doing anything else
			exists, err := namespaceExists(data.Prefix)
			if err != nil {
				log.Errorf("Failed to check if namespace already exists: %v", err)
				return errors.Wrap(err, "Server encountered an error checking if namespace already exists")
			}
			if exists {
				returnMsg := map[string]interface{}{
					"message": fmt.Sprintf("The prefix %s is already registered -- nothing else to do!", data.Prefix),
				}
				ctx.AbortWithStatusJSON(200, returnMsg)
				log.Infof("Skipping registration of prefix %s because it's already registered.", data.Prefix)
				return nil
			}

			reqPrefix, err := validatePrefix(data.Prefix)
			if err != nil {
				err = errors.Wrapf(err, "Requested namespace %s failed validation", reqPrefix)
				log.Errorln(err)
				return err
			}
			data.Prefix = reqPrefix

			valErr, sysErr := validateKeyChaining(reqPrefix, key)
			if valErr != nil {
				log.Errorln(err)
				ctx.JSON(http.StatusForbidden, gin.H{"error": valErr})
				return valErr
			}
			if sysErr != nil {
				log.Errorln(err)
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": sysErr})
				return sysErr
			}

			err = addNamespaceHandler(ctx, data)
			if err != nil {
				ctx.JSON(500, gin.H{"error": "The server encountered an error while attempting to add the prefix to its database"})
				return errors.Wrapf(err, "Failed while trying to add to database")
			}
			return nil
		}
	} else {
		ctx.JSON(500, gin.H{"error": "Server was either unable to verify the client's public key, or an encountered an error with its own"})
		return errors.Errorf("Either the server or the client could not be verified: "+
			"server verified:%t, client verified:%t", serverVerified, clientVerified)
	}
	return nil
}

// Handler functions called upon by the gin router
func cliRegisterNamespace(ctx *gin.Context) {

	var reqData registrationData
	if err := ctx.BindJSON(&reqData); err != nil {
		log.Errorln("Bad request: ", err)
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Bad Request"})
		return
	}

	client := http.Client{Transport: config.GetTransport()}

	if reqData.AccessToken != "" {
		payload := url.Values{}
		payload.Set("access_token", reqData.AccessToken)

		oidcConfig, err := oauth2.ServerOIDCClient()
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server has malformed OIDC configuration"})
			log.Errorf("Failed to load OIDC information for registration with identity: %v", err)
			return
		}

		resp, err := client.PostForm(oidcConfig.Endpoint.UserInfoURL, payload)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error making request to user info endpoint"})
			log.Errorf("Failed to execute post form to user info endpoint %s: %v", oidcConfig.Endpoint.UserInfoURL, err)
			return
		}
		defer resp.Body.Close()

		// Check the status code
		if resp.StatusCode != 200 {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server received non-200 status from user info endpoint"})
			log.Errorf("The user info endpoint %s responded with status code %d", oidcConfig.Endpoint.UserInfoURL, resp.StatusCode)
			return
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Server encountered an error reading response from user info endpoint"})
			log.Errorf("Failed to read body from user info endpoint %s: %v", oidcConfig.Endpoint.UserInfoURL, err)
			return
		}

		reqData.Identity = string(body)
		err = keySignChallenge(ctx, &reqData, "register")
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error during key-sign challenge: " + err.Error()})
			log.Warningf("Failed to complete key sign challenge with identity requirement: %v", err)
		}
		return
	}

	if reqData.IdentityRequired == "false" || reqData.IdentityRequired == "" {
		err := keySignChallenge(ctx, &reqData, "register")
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error during key-sign challenge: " + err.Error()})
			log.Warningf("Failed to complete key sign challenge without identity requirement: %v", err)
		}
		return
	}

	oidcConfig, err := oauth2.ServerOIDCClient()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server has malformed OIDC configuration"})
		log.Errorf("Failed to load OIDC information for registration with identity: %v", err)
		return
	}

	if reqData.DeviceCode == "" {
		log.Debug("Getting Device Code")
		payload := url.Values{}
		payload.Set("client_id", oidcConfig.ClientID)
		payload.Set("client_secret", oidcConfig.ClientSecret)
		payload.Set("scope", strings.Join(oidcConfig.Scopes, " "))

		response, err := client.PostForm(oidcConfig.Endpoint.DeviceAuthURL, payload)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered error requesting device code"})
			log.Errorf("Failed to execute post form to device auth endpoint %s: %v", oidcConfig.Endpoint.DeviceAuthURL, err)
			return
		}
		defer response.Body.Close()

		// Check the response code
		if response.StatusCode != 200 {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server received non-200 status code from OIDC device auth endpoint"})
			log.Errorf("The device auth endpoint %s responded with status code %d", oidcConfig.Endpoint.DeviceAuthURL, response.StatusCode)
			return
		}
		body, err := io.ReadAll(response.Body)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered error reading response from device auth endpoint"})
			log.Errorf("Failed to read body from device auth endpoint %s: %v", oidcConfig.Endpoint.DeviceAuthURL, err)
			return
		}
		var res Response
		err = json.Unmarshal(body, &res)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server could not parse response from device auth endpoint"})
			log.Errorf("Failed to unmarshal body from device auth endpoint %s: %v", oidcConfig.Endpoint.DeviceAuthURL, err)
			return
		}
		verificationURL := res.VerificationURLComplete
		deviceCode := res.DeviceCode
		ctx.JSON(http.StatusOK, gin.H{
			"device_code":      deviceCode,
			"verification_url": verificationURL,
		})
		return
	} else {
		log.Debug("Verifying Device Code")
		payload := url.Values{}
		payload.Set("client_id", oidcConfig.ClientID)
		payload.Set("client_secret", oidcConfig.ClientSecret)
		payload.Set("device_code", reqData.DeviceCode)
		payload.Set("grant_type", "urn:ietf:params:oauth:grant-type:device_code")

		response, err := client.PostForm(oidcConfig.Endpoint.TokenURL, payload)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error while making request to token endpoint"})
			log.Errorf("Failed to execute post form to token endpoint %s: %v", oidcConfig.Endpoint.TokenURL, err)
			return
		}
		defer response.Body.Close()

		// Check the status code
		// We accept either a 200, or a 400.
		if response.StatusCode != 200 && response.StatusCode != 400 {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server received bad status code from token endpoint"})
			log.Errorf("The token endpoint %s responded with status code %d", oidcConfig.Endpoint.TokenURL, response.StatusCode)
			return
		}

		body, err := io.ReadAll(response.Body)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error reading response from token endpoint"})
			log.Errorf("Failed to read body from token endpoint %s: %v", oidcConfig.Endpoint.TokenURL, err)
			return
		}

		var tokenResponse TokenResponse
		err = json.Unmarshal(body, &tokenResponse)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server could not parse error from token endpoint"})
			log.Errorf("Failed to unmarshal body from token endpoint %s: %v", oidcConfig.Endpoint.TokenURL, err)
			return
		}

		// Now we check the status code for a specific case. If it was 400, we check the error in the body
		// to make sure it's "authorization_pending"
		if tokenResponse.AccessToken == "" {
			if response.StatusCode == 400 && tokenResponse.Error == "authorization_pending" {
				ctx.JSON(http.StatusOK, gin.H{
					"status": "PENDING",
				})
			} else {
				ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered unknown error waiting for token"})
				log.Errorf("Token endpoint did not provide a token, and responded with unkown error: %s", string(body))
				return
			}
		} else {
			ctx.JSON(http.StatusOK, gin.H{
				"status":       "APPROVED",
				"access_token": tokenResponse.AccessToken,
			})
		}
		return
	}
}

func addNamespaceHandler(ctx *gin.Context, data *registrationData) error {
	var ns Namespace
	ns.Prefix = data.Prefix

	pubkeyData, err := json.Marshal(data.Pubkey)
	if err != nil {
		return errors.Wrapf(err, "Failed to marshal the pubkey for prefix %s", ns.Prefix)
	}
	ns.Pubkey = string(pubkeyData)
	if data.Identity != "" {
		ns.Identity = data.Identity
	}

	// Overwrite status to Pending to filter malicious request
	ns.AdminMetadata.Status = Pending

	err = addNamespace(&ns)
	if err != nil {
		return errors.Wrapf(err, "Failed to add prefix %s", ns.Prefix)
	}

	ctx.JSON(http.StatusCreated, gin.H{"status": "success"})
	return nil
}

func deleteNamespaceHandler(ctx *gin.Context) {
	/*
		A weird feature of gin is that wildcards always
		add a preceding /. Since the URL parsing that happens
		upstream removes the prefixed / that gets sent, we
		can just leave the one that's added back by the wildcard
		because that reflects the path that's getting stored.
	*/
	prefix := ctx.Param("wildcard")
	log.Debug("Attempting to delete namespace prefix ", prefix)
	if prefix == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "prefix is required to delete"})
		return
	}

	// Check if prefix exists before trying to delete it
	exists, err := namespaceExists(prefix)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error checking if namespace already exists"})
		log.Errorf("Failed to check if the namespace already exists: %v", err)
		return
	}
	if !exists {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "the prefix does not exist so it cannot be deleted"})
		log.Errorln("prefix could not be deleted because it does not exist")
	}

	/*
	*  Need to check that we were provided a token and that it's valid for the origin
	*  TODO: Should we also investigate checking for the token in the url, in case we
	*		 need that option at a later point?
	 */
	authHeader := ctx.GetHeader("Authorization")
	delTokenStr := strings.TrimPrefix(authHeader, "Bearer ")

	// Have the token, now we need to load the JWKS for the prefix
	originJwks, _, err := getNamespaceJwksByPrefix(prefix)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error loading the prefix's stored jwks"})
		log.Errorf("Failed to get prefix's stored jwks: %v", err)
		return
	}

	// Use the JWKS to verify the token -- verification means signature integrity
	parsed, err := jwt.Parse([]byte(delTokenStr), jwt.WithKeySet(originJwks))
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server could not verify/parse the provided deletion token"})
		log.Errorf("Failed to parse the token: %v", err)
		return
	}

	/*
	* The signature is verified, now we need to make sure this token actually gives us
	* permission to delete the namespace from the db. Let's check the subject and the scope.
	* NOTE: The validate function also handles checking `iat` and `exp` to make sure the token
	*       remains valid.
	 */
	scopeValidator := jwt.ValidatorFunc(func(_ context.Context, tok jwt.Token) jwt.ValidationError {
		scope_any, present := tok.Get("scope")
		if !present {
			return jwt.NewValidationError(errors.New("No scope is present; required for authorization"))
		}
		scope, ok := scope_any.(string)
		if !ok {
			return jwt.NewValidationError(errors.New("scope claim in token is not string-valued"))
		}

		for _, scope := range strings.Split(scope, " ") {
			if scope == token_scopes.Pelican_NamespaceDelete.String() {
				return nil
			}
		}
		return jwt.NewValidationError(errors.New("Token does not contain namespace deletion authorization"))
	})
	if err = jwt.Validate(parsed, jwt.WithValidator(scopeValidator)); err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server could not validate the provided deletion token"})
		log.Errorf("Failed to validate the token: %v", err)
		return
	}

	// If we get to this point in the code, we've passed all the security checks and we're ready to delete
	err = deleteNamespace(prefix)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error deleting namespace from database"})
		log.Errorf("Failed to delete namespace from database: %v", err)
		return
	}

	ctx.JSON(http.StatusOK, gin.H{"status": "success"})
}

/**
 * Commenting out until we're ready to use it.  -BB
func cliListNamespaces(c *gin.Context) {
	prefix := c.Param("prefix")
	log.Debugf("Trying to get namespace data for prefix %s", prefix)
	ns, err := getNamespace(prefix)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	c.JSON(http.StatusOK, ns)
}
*/

func getAllNamespacesHandler(ctx *gin.Context) {
	nss, err := getAllNamespaces()
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error trying to list all namespaces"})
		log.Errorln("Failed to get all namespaces: ", err)
		return
	}
	ctx.JSON(http.StatusOK, nss)
}

// Gin requires no wildcard match and exact match fall under the same
// parent path, so we need to handle all routing under "/" route ourselves.
//
// See https://github.com/PelicanPlatform/pelican/issues/566
func wildcardHandler(ctx *gin.Context) {
	// A weird feature of gin is that wildcards always
	// add a preceding /. Since the prefix / was trimmed
	// out during the url parsing, we can just leave the
	// new / here!
	path := ctx.Param("wildcard")

	// Get the prefix's JWKS
	// Avoid using filepath.Base for path matching, as filepath format depends on OS
	// while HTTP path is always slash (/)
	if strings.HasSuffix(path, "/.well-known/issuer.jwks") {
		prefix := strings.TrimSuffix(path, "/.well-known/issuer.jwks")
		found, err := namespaceExistsByPrefix(prefix)
		if err != nil {
			log.Error("Error checking if prefix ", prefix, " exists: ", err)
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error trying to check if the namespace exists"})
			return
		}
		if !found {
			ctx.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("namespace prefix '%s', was not found", prefix)})
			return
		}

		jwks, adminMetadata, err := getNamespaceJwksByPrefix(prefix)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "server encountered an error trying to get jwks for prefix"})
			log.Errorf("Failed to load jwks for prefix %s: %v", prefix, err)
			return
		}
		if adminMetadata != nil && adminMetadata.Status != Approved {
			if strings.HasPrefix(prefix, "/caches/") { // Caches
				if param.Registry_RequireCacheApproval.GetBool() {
					// Use 403 to distinguish between server error
					ctx.JSON(http.StatusForbidden, gin.H{"error": "The cache has not been approved by federation administrator"})
					return
				}
			} else { // Origins
				if param.Registry_RequireOriginApproval.GetBool() {
					// Use 403 to distinguish between server error
					ctx.JSON(http.StatusForbidden, gin.H{"error": "The origin has not been approved by federation administrator"})
					return
				}
			}
		}
		ctx.JSON(http.StatusOK, jwks)
		return
	} else if strings.HasSuffix(path, "/.well-known/openid-configuration") {
		// Check that the namespace exists before constructing config JSON
		prefix := strings.TrimSuffix(path, "/.well-known/openid-configuration")
		exists, err := namespaceExists(prefix)
		if err != nil {
			ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Server encountered an error while checking if the prefix exists"})
			log.Errorf("Error while checking for existence of prefix %s: %v", prefix, err)
			return
		}
		if !exists {
			ctx.JSON(http.StatusNotFound, gin.H{"error": fmt.Sprintf("The requested prefix %s does not exist in the registry's database", prefix)})
		}
		// Construct the openid-configuration JSON and return to the requester
		// For a given namespace "foo", the jwks should be located at <registry url>/api/v1.0/registry/foo/.well-known/issuer.jwks
		configUrl, err := url.Parse(param.Server_ExternalWebUrl.GetString())
		if err != nil {
			log.Errorf("Failed to parse configured external web URL while constructing namespace jwks location: %v", err)
			return
		}

		path := strings.TrimSuffix(path, "/openid-configuration")
		configUrl.Path, err = url.JoinPath("api", "v1.0", "registry", path, "issuer.jwks")
		if err != nil {
			log.Errorf("Failed to construct namespace jwks URL: %v", err)
			return
		}

		nsCfg := NamespaceConfig{
			JwksUri: configUrl.String(),
		}

		ctx.JSON(http.StatusOK, nsCfg)
		return
	} else {

		ctx.String(http.StatusNotFound, "404 Page not found")
		return
	}
}

// Check if a namespace prefix exists and its public key matches the registry record
func checkNamespaceExistsHandler(ctx *gin.Context) {
	req := checkNamespaceExistsReq{}
	if err := ctx.ShouldBind(&req); err != nil {
		log.Debug("Failed to parse request body for namespace exits check: ", err)
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse request body"})
		return
	}
	if req.Prefix == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "prefix is required"})
		return
	}
	if req.PubKey == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "pubkey is required"})
		return
	}
	jwksReq, err := jwk.ParseString(req.PubKey)
	if err != nil {
		log.Debug("pubkey is not a valid JWK string:", req.PubKey, err)
		ctx.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("pubkey is not a valid JWK string: %s", req.PubKey)})
		return
	}
	if jwksReq.Len() != 1 {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("pubkey is a jwks with multiple or zero key: %s", req.PubKey)})
		return
	}
	jwkReq, exists := jwksReq.Key(0)
	if !exists {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": fmt.Sprintf("the first key from the pubkey does not exist: %s", req.PubKey)})
		return
	}

	found, err := namespaceExistsByPrefix(req.Prefix)
	if err != nil {
		log.Debugln("Failed to check if namespace exists by prefix", err)
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Failed to check if the namespace exists"})
		return
	}
	if !found {
		// We return 200 even with prefix not found so that 404 can be used to check if the route exists (OSDF)
		// and fallback to OSDF way of checking if we do get 404
		res := checkNamespaceExistsRes{PrefixExists: false, Message: "Prefix was not found in database"}
		ctx.JSON(http.StatusOK, res)
		return
	}
	// Just to check if the key matches. We don't care about approval status
	jwksDb, _, err := getNamespaceJwksByPrefix(req.Prefix)
	if err != nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}

	registryKey, isPresent := jwksDb.LookupKeyID(jwkReq.KeyID())
	if !isPresent {
		res := checkNamespaceExistsRes{PrefixExists: true, KeyMatch: false, Message: "Given JWK is not present in the JWKS from database"}
		ctx.JSON(http.StatusOK, res)
		return
	} else if jwk.Equal(registryKey, jwkReq) {
		res := checkNamespaceExistsRes{PrefixExists: true, KeyMatch: true}
		ctx.JSON(http.StatusOK, res)
		return
	} else {
		res := checkNamespaceExistsRes{PrefixExists: true, KeyMatch: false, Message: "Given JWK does not equal to the JWK from database"}
		ctx.JSON(http.StatusOK, res)
		return
	}
}

func checkNamespaceStatusHandler(ctx *gin.Context) {
	req := checkStatusReq{}
	if err := ctx.ShouldBind(&req); err != nil {
		log.Debug("Failed to parse request body for namespace status check: ", err)
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "Failed to parse request body"})
		return
	}
	if req.Prefix == "" {
		ctx.JSON(http.StatusBadRequest, gin.H{"error": "prefix is required"})
		return
	}
	ns, err := getNamespaceByPrefix(req.Prefix)
	if err != nil || ns == nil {
		ctx.JSON(http.StatusInternalServerError, gin.H{"error": "Error getting namespace"})
		return
	}
	emptyMetadata := AdminMetadata{}
	// If Registry.RequireCacheApproval or Registry.RequireOriginApproval is false
	// we return Approved == true
	if ns.AdminMetadata != emptyMetadata {
		// Caches
		if strings.HasPrefix(req.Prefix, "/caches") && param.Registry_RequireCacheApproval.GetBool() {
			res := checkStatusRes{Approved: ns.AdminMetadata.Status == Approved}
			ctx.JSON(http.StatusOK, res)
			return
		} else if !param.Registry_RequireCacheApproval.GetBool() {
			res := checkStatusRes{Approved: true}
			ctx.JSON(http.StatusOK, res)
			return
		} else {
			// Origins
			if param.Registry_RequireOriginApproval.GetBool() {
				res := checkStatusRes{Approved: ns.AdminMetadata.Status == Approved}
				ctx.JSON(http.StatusOK, res)
				return
			} else {
				res := checkStatusRes{Approved: true}
				ctx.JSON(http.StatusOK, res)
				return
			}
		}
	} else {
		// For legacy Pelican (<=7.3.0) registry schema without Admin_Metadata
		res := checkStatusRes{Approved: true}
		ctx.JSON(http.StatusOK, res)
	}
}

func RegisterRegistryAPI(router *gin.RouterGroup) {
	registryAPI := router.Group("/api/v1.0/registry")

	// DO NOT add any other GET route with path starts with "/" to registryAPI
	// It will cause duplicated route error. Use wildcardHandler to handle such
	// routing if needed.
	{
		registryAPI.POST("", cliRegisterNamespace)
		registryAPI.GET("", getAllNamespacesHandler)

		// Handle everything under "/" route with GET method
		registryAPI.GET("/*wildcard", wildcardHandler)
		registryAPI.POST("/checkNamespaceExists", checkNamespaceExistsHandler)
		registryAPI.POST("/checkNamespaceStatus", checkNamespaceStatusHandler)
		registryAPI.DELETE("/*wildcard", deleteNamespaceHandler)
	}
}
