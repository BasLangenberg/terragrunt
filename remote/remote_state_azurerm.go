package remote

import (
	"context"
	"fmt"
	"reflect"
	"strconv"
	"time"

	"github.com/Azure/azure-sdk-for-go/services/storage/mgmt/2021-02-01/storage"
	"github.com/Azure/go-autorest/autorest/azure/auth"
	"github.com/mitchellh/mapstructure"

	"github.com/gruntwork-io/terragrunt/errors"
	"github.com/gruntwork-io/terragrunt/options"
	"github.com/gruntwork-io/terragrunt/util"
)

/*
 * We use this construct to separate the config key 'azurerm_storage_account_tags' from the others, as they
 * are specific to the azurerm backend, but only used by terragrunt to tag the azurerm storage account in case it
 * has to create them.
 */
type ExtendedRemoteStateConfigAzureRM struct {
	remoteStateConfigAzureRM RemoteStateConfigAzureRM
	Location                 string            `mapstructure:"location"`
	Tags                     map[string]string `mapstructure:"tags"`
	SKU                      string            `mapstructure:"sku"`
	Kind                     string            `mapstructure:"kind"`
	AccessTier               string            `mapstructure:"access_tier"`
	SkipVersioning           bool              `mapstructure:"skip_versioning"`
	SkipCreate               bool              `mapstructure:"skip_create"`
	SkipAzureRBAC            bool              `mapstructure:"skip_azure_rbac"`
}

// A representation of the configuration options available for AzureRM remote state
// See: https://www.terraform.io/docs/language/settings/backends/azurerm.html
type RemoteStateConfigAzureRM struct {
	// All these options configure the storage account, container, and blob
	TenantId           string `mapstructure:"tenant_id"`
	SubscriptionId     string `mapstructure:"subscription_id"`
	ResourceGroupName  string `mapstructure:"resource_group_name"`
	StorageAccountName string `mapstructure:"storage_account_name"`
	ContainerName      string `mapstructure:"container_name"`
	Snapshot           bool   `mapstructure:"snapshot"`
	Key                string `mapstructure:"key"`

	// All these options support auth
	UseMSI                    string `mapstructure:"use_msi"`
	MSIEndpoint               string `mapstructure:"msi_endpoint"`
	UseAzureADAuth            bool   `mapstructure:"use_azuread_auth"`
	AccessKey                 string `mapstructure:"access_key"`
	SASToken                  string `mapstructure:"sas_token"`
	ClientId                  string `mapstructure:"client_id"`
	ClientSecret              string `mapstructure:"client_secret"`
	ClientCertificatePassword string `mapstructure:"client_certificate_password"`
	ClientCertificatePath     string `mapstructure:"client_certificate_path"`

	Endpoint    string `mapstructure:"endpoint"`    // Set when using Azure Stack
	Environment string `mapstructure:"environment"` // Set when using an environment other than "public"
}

// These are settings that can appear in the remote_state config that are ONLY used by Terragrunt and NOT forwarded
// to the underlying Terraform backend configuration.
var terragruntAzureRMOnlyConfigs = []string{
	"location",
	"tags",
	"sku",
	"kind",
	"access_tier",
	"skip_versioning",
	"skip_create",
	"skip_azure_rbac",
}

const MAX_RETRIES_WAITING_FOR_AZURE_RM_BUCKET = 12
const SLEEP_BETWEEN_RETRIES_WAITING_FOR_AZURE_RM_BUCKET = 5 * time.Second

type AzureRMInitializer struct{}

// Returns true if:
//
// 1. Any of the existing backend settings are different than the current config
// 2. The configured AzureRM storage account does not exist
func (armInitializer AzureRMInitializer) NeedsInitialization(remoteState *RemoteState, existingBackend *TerraformBackend, terragruntOptions *options.TerragruntOptions) (bool, error) {
	if remoteState.DisableInit {
		return false, nil
	}

	armConfig, err := parseAzureRMConfig(remoteState.Config)
	if err != nil {
		return false, err
	}

	armClient, err := CreateAzureRMClient(*armConfig)
	if err != nil {
		return false, err
	}

	exists, err := DoesStorageAccountExist(armClient, armConfig)
	if err != nil {
		return false, err
	}

	if !exists {
		return true, nil
	}

	return false, nil
}

// Return true if the given config is in any way different than what is configured for the backend
func armConfigValuesEqual(config map[string]interface{}, existingBackend *TerraformBackend, terragruntOptions *options.TerragruntOptions) bool {
	if existingBackend == nil {
		return len(config) == 0
	}

	if existingBackend.Type != "azurerm" {
		terragruntOptions.Logger.Debugf("Backend type has changed from azurerm to %s", existingBackend.Type)
		return false
	}

	if len(config) == 0 && len(existingBackend.Config) == 0 {
		return true
	}

	// If other keys in config are bools, DeepEqual also will consider the maps to be different.
	for key, value := range existingBackend.Config {
		if util.KindOf(existingBackend.Config[key]) == reflect.String && util.KindOf(config[key]) == reflect.Bool {
			if convertedValue, err := strconv.ParseBool(value.(string)); err == nil {
				existingBackend.Config[key] = convertedValue
			}
		}
	}

	// Construct a new map excluding custom GCS labels that are only used in Terragrunt config and not in Terraform's backend
	comparisonConfig := make(map[string]interface{})
	for key, value := range config {
		comparisonConfig[key] = value
	}

	for _, key := range terragruntAzureRMOnlyConfigs {
		delete(comparisonConfig, key)
	}

	if !terraformStateConfigEqual(existingBackend.Config, comparisonConfig) {
		terragruntOptions.Logger.Debugf("Backend config changed from %s to %s", existingBackend.Config, config)
		return false
	}

	return true
}

// Initialize the remote state AzureRM storage account specified in the given config. This function will validate the config
// parameters, create the AzureRM storage account if it doesn't already exist, and check that versioning is enabled.
func (armInitializer AzureRMInitializer) Initialize(remoteState *RemoteState, terragruntOptions *options.TerragruntOptions) error {
	armConfigExtended, err := parseExtendedAzureRMConfig(remoteState.Config)
	if err != nil {
		return err
	}

	if err := validateAzureRMConfig(armConfigExtended, terragruntOptions); err != nil {
		return err
	}

	var armConfig = armConfigExtended.remoteStateConfigAzureRM

	armClient, err := CreateAzureRMClient(armConfig)

	//armClient, err := CreateAzureRMClient(armConfig)
	if err != nil {
		return err
	}

	// If storage_account_name is specified and skip_create is false then check if the Storage Account needs to be created
	if !armConfigExtended.SkipCreate && armConfig.StorageAccountName != "" {
		if err := createStorageAccountIfNecessary(armClient, armConfigExtended, terragruntOptions); err != nil {
			return err
		}
	}

	// If bucket is specified and skip_bucket_versioning is false then warn user if versioning is disabled on bucket
	if !armConfigExtended.SkipVersioning && armConfig.StorageAccountName != "" {
		if err := checkIfAzureRMVersioningEnabled(armClient, &armConfig, terragruntOptions); err != nil {
			return err
		}
	}

	return nil
}

func (armInitializer AzureRMInitializer) GetTerraformInitArgs(config map[string]interface{}) map[string]interface{} {
	var filteredConfig = make(map[string]interface{})

	for key, val := range config {
		if util.ListContainsElement(terragruntAzureRMOnlyConfigs, key) {
			continue
		}

		filteredConfig[key] = val
	}

	return filteredConfig
}

// Parse the given map into a AzureRM config
func parseAzureRMConfig(config map[string]interface{}) (*RemoteStateConfigAzureRM, error) {
	var armConfig RemoteStateConfigAzureRM
	if err := mapstructure.Decode(config, &armConfig); err != nil {
		return nil, errors.WithStackTrace(err)
	}
	return &armConfig, nil
}

// Parse the given map into a AzureRM config
func parseExtendedAzureRMConfig(config map[string]interface{}) (*ExtendedRemoteStateConfigAzureRM, error) {
	var armConfig RemoteStateConfigAzureRM
	var extendedConfig ExtendedRemoteStateConfigAzureRM

	if err := mapstructure.Decode(config, &armConfig); err != nil {
		return nil, errors.WithStackTrace(err)
	}

	if err := mapstructure.Decode(config, &extendedConfig); err != nil {
		return nil, errors.WithStackTrace(err)
	}

	extendedConfig.remoteStateConfigAzureRM = armConfig

	return &extendedConfig, nil
}

// Validate all the parameters of the given AzureRM remote state configuration
func validateAzureRMConfig(extendedConfig *ExtendedRemoteStateConfigAzureRM, terragruntOptions *options.TerragruntOptions) error {
	var config = extendedConfig.remoteStateConfigAzureRM

	if config.StorageAccountName == "" {
		return errors.WithStackTrace(MissingRequiredAzureRMRemoteStateConfig("prefix"))
	}

	return nil
}

// If the storage account specified in the given config doesn't already exist, prompt the user to create it, and if the user
// confirms, create the storage account and enable versioning for it.
func createStorageAccountIfNecessary(armClient *storage.AccountsClient, config *ExtendedRemoteStateConfigAzureRM, terragruntOptions *options.TerragruntOptions) error {
	return nil
}

// Check if versioning is enabled for the AzureRM storage account specified in the given config and warn the user if it is not
func checkIfAzureRMVersioningEnabled(armClient *storage.AccountsClient, config *RemoteStateConfigAzureRM, terragruntOptions *options.TerragruntOptions) error {
	return nil
}

// createStorageAccountWithVersioning creates the given AzureRM storage account and enables versioning for it.
func createStorageAccountWithVersioning(armClient *storage.AccountsClient, config *ExtendedRemoteStateConfigAzureRM, terragruntOptions *options.TerragruntOptions) error {
	return nil
}

func AddLabelsToAzureRMBucket(armClient *storage.AccountsClient, config *ExtendedRemoteStateConfigAzureRM, terragruntOptions *options.TerragruntOptions) error {
	return nil
}

// Create the AzureRM storage account specified in the given config
func CreateAzureRMBucket(armClient *storage.AccountsClient, config *ExtendedRemoteStateConfigAzureRM, terragruntOptions *options.TerragruntOptions) error {
	return nil
}

// GCP is eventually consistent, so after creating a AzureRM storage account, this method can be used to wait until the information
// about that AzureRM storage account has propagated everywhere.
func WaitUntilAzureRMBucketExists(armClient *storage.AccountsClient, config *RemoteStateConfigAzureRM, terragruntOptions *options.TerragruntOptions) error {
	return nil
}

// DoesStorageAccountExist returns true if the AzureRM storage account specified in the given config exists and the current user has the
// ability to access it.
func DoesStorageAccountExist(armClient *storage.AccountsClient, config *RemoteStateConfigAzureRM) (bool, error) {
	ctx := context.Background()

	accountCheckNameAvailabilityParameters := storage.AccountCheckNameAvailabilityParameters{
		Name: &config.StorageAccountName,
	}

	result, err := armClient.CheckNameAvailability(ctx, accountCheckNameAvailabilityParameters)
	if err != nil {
		// If the name check fails, we'll assume the storage account exists by returning true.
		// The error will contain the reason the name check failed, which will be returned to the caller.
		return true, err
	}

	if *result.NameAvailable {
		return false, nil
	}

	return true, nil
}

// CreateAzureRMClient creates an authenticated client for AzureRM
func CreateAzureRMClient(armConfigRemote RemoteStateConfigAzureRM) (*storage.AccountsClient, error) {
	authorizer, err := auth.NewAuthorizerFromEnvironment()
	if err != nil {
		return nil, err
	}
	storageAccountsClient := storage.NewAccountsClient(armConfigRemote.SubscriptionId)
	storageAccountsClient.Authorizer = authorizer
	storageAccountsClient.AddToUserAgent("terragrunt-cli")

	return &storageAccountsClient, nil
}

// Custom error types

type MissingRequiredAzureRMRemoteStateConfig string

func (configName MissingRequiredAzureRMRemoteStateConfig) Error() string {
	return fmt.Sprintf("Missing required AzureRM remote state configuration %s", string(configName))
}

func Coalesce(strings ...string) (string, error) {
	for _, str := range strings {
		if str != "" {
			return str, nil
		}
	}

	return "", fmt.Errorf("Coalesce failed, no non-empty string values were given")
}
