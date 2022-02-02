/*
Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package keyvault

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"regexp"
	"strings"

	"github.com/Azure/azure-sdk-for-go/profiles/latest/keyvault/keyvault"
	kvauth "github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/tidwall/gjson"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	esv1alpha2 "github.com/external-secrets/external-secrets/apis/externalsecrets/v1alpha2"
	smmeta "github.com/external-secrets/external-secrets/apis/meta/v1"
	"github.com/external-secrets/external-secrets/pkg/provider"
	"github.com/external-secrets/external-secrets/pkg/provider/schema"
)

const (
	defaultObjType = "secret"
	vaultResource  = "https://vault.azure.net"
)

// interface to keyvault.BaseClient.
type SecretClient interface {
	GetKey(ctx context.Context, vaultBaseURL string, keyName string, keyVersion string) (result keyvault.KeyBundle, err error)
	GetSecret(ctx context.Context, vaultBaseURL string, secretName string, secretVersion string) (result keyvault.SecretBundle, err error)
	GetSecretsComplete(ctx context.Context, vaultBaseURL string, maxresults *int32) (result keyvault.SecretListResultIterator, err error)
	GetCertificate(ctx context.Context, vaultBaseURL string, certificateName string, certificateVersion string) (result keyvault.CertificateBundle, err error)
}

type Azure struct {
	kube       client.Client
	store      esv1alpha2.GenericStore
	baseClient SecretClient
	vaultURL   string
	namespace  string
}

func init() {
	schema.Register(&Azure{}, &esv1alpha2.SecretStoreProvider{
		AzureKV: &esv1alpha2.AzureKVProvider{},
	})
}

// NewClient constructs a new secrets client based on the provided store.
func (a *Azure) NewClient(ctx context.Context, store esv1alpha2.GenericStore, kube client.Client, namespace string) (provider.SecretsClient, error) {
	return newClient(ctx, store, kube, namespace)
}

func newClient(ctx context.Context, store esv1alpha2.GenericStore, kube client.Client, namespace string) (provider.SecretsClient, error) {
	anAzure := &Azure{
		kube:      kube,
		store:     store,
		namespace: namespace,
	}

	clientSet, err := anAzure.setAzureClientWithManagedIdentity()
	if clientSet {
		return anAzure, err
	}

	clientSet, err = anAzure.setAzureClientWithServicePrincipal(ctx)
	if clientSet {
		return anAzure, err
	}

	return nil, fmt.Errorf("cannot initialize Azure Client: no valid authType was specified")
}

// Implements store.Client.GetSecret Interface.
// Retrieves a secret/Key/Certificate with the secret name defined in ref.Name
// The Object Type is defined as a prefix in the ref.Name , if no prefix is defined , we assume a secret is required.
func (a *Azure) GetSecret(ctx context.Context, ref esv1alpha2.ExternalSecretDataRemoteRef) ([]byte, error) {
	version := ""
	basicClient := a.baseClient
	objectType, secretName := getObjType(ref)

	if secretName == "" {
		return nil, fmt.Errorf("%s name cannot be empty", objectType)
	}

	if ref.Version != "" {
		version = ref.Version
	}

	switch objectType {
	case defaultObjType:
		// returns a SecretBundle with the secret value
		// https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/services/keyvault/v7.0/keyvault#SecretBundle
		secretResp, err := basicClient.GetSecret(context.Background(), a.vaultURL, secretName, version)
		if err != nil {
			return nil, err
		}
		if ref.Property == "" {
			return []byte(*secretResp.Value), nil
		}
		res := gjson.Get(*secretResp.Value, ref.Property)
		if !res.Exists() {
			return nil, fmt.Errorf("property %s does not exist in key %s", ref.Property, ref.Key)
		}
		return []byte(res.String()), err
	case "cert":
		// returns a CertBundle. We return CER contents of x509 certificate
		// see: https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/services/keyvault/v7.0/keyvault#CertificateBundle
		secretResp, err := basicClient.GetCertificate(context.Background(), a.vaultURL, secretName, version)
		if err != nil {
			return nil, err
		}
		return *secretResp.Cer, nil
	case "key":
		// returns a KeyBundle that contains a jwk
		// azure kv returns only public keys
		// see: https://pkg.go.dev/github.com/Azure/azure-sdk-for-go/services/keyvault/v7.0/keyvault#KeyBundle
		keyResp, err := basicClient.GetKey(context.Background(), a.vaultURL, secretName, version)
		if err != nil {
			return nil, err
		}
		return json.Marshal(keyResp.Key)
	}

	return nil, fmt.Errorf("unknown Azure Keyvault object Type for %s", secretName)
}

// Implements store.Client.GetSecretMap Interface.
// New version of GetSecretMap.
func (a *Azure) GetSecretMap(ctx context.Context, ref esv1alpha2.ExternalSecretDataFromRemoteRef) (map[string][]byte, error) {
	dataRef := ref.GetDataRemoteRef()
	objectType, secretName := getObjType(dataRef)

	switch objectType {
	case defaultObjType:
		data, err := a.GetSecret(ctx, dataRef)
		if err != nil {
			return nil, err
		}

		kv := make(map[string]string)
		err = json.Unmarshal(data, &kv)
		if err != nil {
			return nil, fmt.Errorf("error unmarshalling json data: %w", err)
		}

		secretData := make(map[string][]byte)
		for k, v := range kv {
			secretData[k] = []byte(v)
		}

		return secretData, nil
	case "cert":
		return nil, fmt.Errorf("cannot get use dataFrom to get certificate secret")
	case "key":
		return nil, fmt.Errorf("cannot get use dataFrom to get key secret")
	}

	return nil, fmt.Errorf("unknown Azure Keyvault object Type for %s", secretName)
}

// Implements store.Client.GetAllSecrets Interface.
// New version of GetAllSecrets.
func (a *Azure) GetAllSecrets(ctx context.Context, ref esv1alpha2.ExternalSecretDataFromRemoteRef) (map[string][]byte, error) {
	basicClient := a.baseClient
	secretsMap := make(map[string][]byte)
	checkTags := len(ref.Find.Tags) > 0
	checkName := len(ref.Find.Name.RegExp) > 0

	secretListIter, err := basicClient.GetSecretsComplete(context.Background(), a.vaultURL, nil)

	if err != nil {
		return nil, err
	}
	for secretListIter.NotDone() {
		secretList := secretListIter.Response().Value
		for _, secret := range *secretList {
			ok, secretName := isValidSecret(checkTags, checkName, ref, secret)
			if !ok {
				continue
			}

			secretResp, err := basicClient.GetSecret(context.Background(), a.vaultURL, secretName, "")
			secretValue := *secretResp.Value

			if err != nil {
				return nil, err
			}
			secretsMap[secretName] = []byte(secretValue)
		}
		err = secretListIter.Next()
		if err != nil {
			return nil, err
		}
	}
	return secretsMap, nil
}

func isValidSecret(checkTags, checkName bool, ref esv1alpha2.ExternalSecretDataFromRemoteRef, secret keyvault.SecretItem) (bool, string) {
	if secret.ID == nil || !*secret.Attributes.Enabled {
		return false, ""
	}

	if checkTags && !okByTags(ref, secret) {
		return false, ""
	}

	secretName := path.Base(*secret.ID)
	if checkName && !okByName(ref, secretName) {
		return false, ""
	}

	return true, secretName
}

func okByName(ref esv1alpha2.ExternalSecretDataFromRemoteRef, secretName string) bool {
	matches, _ := regexp.MatchString(ref.Find.Name.RegExp, secretName)
	return matches
}

func okByTags(ref esv1alpha2.ExternalSecretDataFromRemoteRef, secret keyvault.SecretItem) bool {
	tagsFound := true
	for k, v := range ref.Find.Tags {
		if val, ok := secret.Tags[k]; !ok || *val != v {
			tagsFound = false
			break
		}
	}
	return tagsFound
}

func (a *Azure) setAzureClientWithManagedIdentity() (bool, error) {
	spec := *a.store.GetSpec().Provider.AzureKV

	if *spec.AuthType != esv1alpha2.ManagedIdentity {
		return false, nil
	}

	msiConfig := kvauth.NewMSIConfig()
	msiConfig.Resource = vaultResource
	if spec.IdentityID != nil {
		msiConfig.ClientID = *spec.IdentityID
	}
	authorizer, err := msiConfig.Authorizer()
	if err != nil {
		return true, err
	}

	basicClient := keyvault.New()
	basicClient.Authorizer = authorizer

	a.baseClient = basicClient
	a.vaultURL = *spec.VaultURL

	return true, nil
}

func (a *Azure) setAzureClientWithServicePrincipal(ctx context.Context) (bool, error) {
	spec := *a.store.GetSpec().Provider.AzureKV

	if *spec.AuthType != esv1alpha2.ServicePrincipal {
		return false, nil
	}

	if spec.TenantID == nil {
		return true, fmt.Errorf("missing tenantID in store config")
	}
	if spec.AuthSecretRef == nil {
		return true, fmt.Errorf("missing clientID/clientSecret in store config")
	}
	if spec.AuthSecretRef.ClientID == nil || spec.AuthSecretRef.ClientSecret == nil {
		return true, fmt.Errorf("missing accessKeyID/secretAccessKey in store config")
	}
	clusterScoped := false
	if a.store.GetObjectKind().GroupVersionKind().Kind == esv1alpha2.ClusterSecretStoreKind {
		clusterScoped = true
	}
	cid, err := a.secretKeyRef(ctx, a.store.GetNamespace(), *spec.AuthSecretRef.ClientID, clusterScoped)
	if err != nil {
		return true, err
	}
	csec, err := a.secretKeyRef(ctx, a.store.GetNamespace(), *spec.AuthSecretRef.ClientSecret, clusterScoped)
	if err != nil {
		return true, err
	}

	clientCredentialsConfig := kvauth.NewClientCredentialsConfig(cid, csec, *spec.TenantID)
	clientCredentialsConfig.Resource = vaultResource
	authorizer, err := clientCredentialsConfig.Authorizer()
	if err != nil {
		return true, err
	}

	basicClient := keyvault.New()
	basicClient.Authorizer = authorizer

	a.baseClient = &basicClient
	a.vaultURL = *spec.VaultURL

	return true, nil
}

func (a *Azure) secretKeyRef(ctx context.Context, namespace string, secretRef smmeta.SecretKeySelector, clusterScoped bool) (string, error) {
	var secret corev1.Secret
	ref := types.NamespacedName{
		Namespace: namespace,
		Name:      secretRef.Name,
	}
	if clusterScoped && secretRef.Namespace != nil {
		ref.Namespace = *secretRef.Namespace
	}
	err := a.kube.Get(ctx, ref, &secret)
	if err != nil {
		return "", fmt.Errorf("could not find secret %s/%s: %w", ref.Namespace, ref.Name, err)
	}
	keyBytes, ok := secret.Data[secretRef.Key]
	if !ok {
		return "", fmt.Errorf("no data for %q in secret '%s/%s'", secretRef.Key, secretRef.Name, namespace)
	}
	value := strings.TrimSpace(string(keyBytes))
	return value, nil
}

func (a *Azure) Close(ctx context.Context) error {
	return nil
}

func getObjType(ref esv1alpha2.ExternalSecretDataRemoteRef) (string, string) {
	objectType := defaultObjType

	secretName := ref.Key
	nameSplitted := strings.Split(secretName, "/")

	if len(nameSplitted) > 1 {
		objectType = nameSplitted[0]
		secretName = nameSplitted[1]
		// TODO: later tokens can be used to read the secret tags
	}
	return objectType, secretName
}
