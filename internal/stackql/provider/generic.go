package provider

import (
	"errors"
	"fmt"

	"github.com/stackql/stackql/internal/stackql/constants"
	"github.com/stackql/stackql/internal/stackql/discovery"
	"github.com/stackql/stackql/internal/stackql/docparser"
	"github.com/stackql/stackql/internal/stackql/dto"
	sdk "github.com/stackql/stackql/internal/stackql/google_sdk"
	"github.com/stackql/stackql/internal/stackql/httpexec"
	"github.com/stackql/stackql/internal/stackql/methodselect"
	"github.com/stackql/stackql/internal/stackql/netutils"
	"github.com/stackql/stackql/internal/stackql/relational"
	"github.com/stackql/stackql/internal/stackql/sqlengine"

	"github.com/stackql/stackql/internal/pkg/sqltypeutil"

	"github.com/stackql/go-openapistackql/openapistackql"

	"net/http"
	"net/url"
	"regexp"
	"strings"

	log "github.com/sirupsen/logrus"
)

type GenericProvider struct {
	provider         *openapistackql.Provider
	runtimeCtx       dto.RuntimeCtx
	currentService   string
	discoveryAdapter discovery.IDiscoveryAdapter
	apiVersion       string
	methodSelector   methodselect.IMethodSelector
}

func (gp *GenericProvider) getDefaultKeyForSelectItems(sc *openapistackql.Schema) string {
	if gp.provider.SelectItemsKey != "" {
		return gp.provider.SelectItemsKey
	}
	return "items"
}

func (gp *GenericProvider) GetDiscoveryGeneration(dbEngine sqlengine.SQLEngine) (int, error) {
	return dbEngine.GetCurrentDiscoveryGenerationId(gp.GetProviderString())
}

func (gp *GenericProvider) GetDefaultKeyForDeleteItems() string {
	if gp.provider.DeleteItemsKey != "" {
		return gp.provider.DeleteItemsKey
	}
	return "items"
}

func (gp *GenericProvider) GetMethodSelector() methodselect.IMethodSelector {
	return gp.methodSelector
}

func (gp *GenericProvider) GetVersion() string {
	return gp.apiVersion
}

func (gp *GenericProvider) GetServiceShard(serviceKey string, resourceKey string, runtimeCtx dto.RuntimeCtx) (*openapistackql.Service, error) {
	return gp.discoveryAdapter.GetServiceShard(gp.provider, serviceKey, resourceKey)
}

func (gp *GenericProvider) inferAuthType(authCtx dto.AuthCtx, authTypeRequested string) string {
	ft := strings.ToLower(authTypeRequested)
	switch ft {
	case dto.AuthApiKeyStr:
		return dto.AuthApiKeyStr
	case dto.AuthServiceAccountStr:
		return dto.AuthServiceAccountStr
	case dto.AuthInteractiveStr:
		return dto.AuthInteractiveStr
	}
	if authCtx.KeyFilePath != "" || authCtx.KeyEnvVar != "" {
		return dto.AuthServiceAccountStr
	}
	return dto.AuthInteractiveStr
}

func (gp *GenericProvider) Auth(authCtx *dto.AuthCtx, authTypeRequested string, enforceRevokeFirst bool) (*http.Client, error) {
	switch gp.inferAuthType(*authCtx, authTypeRequested) {
	case dto.AuthApiKeyStr:
		return gp.apiTokenFileAuth(authCtx)
	case dto.AuthServiceAccountStr:
		return gp.keyFileAuth(authCtx)
	case dto.AuthInteractiveStr:
		return gp.oAuth(authCtx, enforceRevokeFirst)
	}
	return nil, fmt.Errorf("Could not infer auth type")
}

func (gp *GenericProvider) AuthRevoke(authCtx *dto.AuthCtx) error {
	switch strings.ToLower(authCtx.Type) {
	case dto.AuthServiceAccountStr:
		return errors.New(constants.ServiceAccountRevokeErrStr)
	case dto.AuthInteractiveStr:
		err := sdk.RevokeGoogleAuth()
		if err == nil {
			deactivateAuth(authCtx)
		}
		return err
	}
	return fmt.Errorf(`Auth revoke for Google Failed; improper auth method: "%s" speciied`, authCtx.Type)
}

func (gp *GenericProvider) GetMethodForAction(serviceName string, resourceName string, iqlAction string, runtimeCtx dto.RuntimeCtx) (*openapistackql.OperationStore, string, error) {
	rsc, err := gp.GetResource(serviceName, resourceName, runtimeCtx)
	if err != nil {
		return nil, "", err
	}
	return gp.methodSelector.GetMethodForAction(rsc, iqlAction)
}

func (gp *GenericProvider) InferDescribeMethod(rsc *openapistackql.Resource) (*openapistackql.OperationStore, string, error) {
	if rsc == nil {
		return nil, "", fmt.Errorf("cannot infer describe method from nil resource")
	}
	var method *openapistackql.OperationStore
	m, methodErr := rsc.FindMethod("list")
	if methodErr == nil && m != nil {
		return m, "list", nil
	}
	m, methodErr = rsc.FindMethod("aggregatedList")
	if methodErr == nil && m != nil {
		return m, "aggregatedList", nil
	}
	m, methodErr = rsc.FindMethod("get")
	if methodErr == nil && m != nil {
		return m, "get", nil
	}
	var ms []string
	for _, v := range rsc.Methods {
		vp := &v
		ms = append(ms, v.GetName())
		if strings.HasPrefix(v.GetName(), "get") {
			method = vp
			return method, v.GetName(), nil
		}
	}
	for _, v := range rsc.Methods {
		vp := &v
		if strings.HasPrefix(v.GetName(), "list") {
			method = vp
			return method, v.GetName(), nil
		}
	}
	return nil, "", fmt.Errorf("SELECT not supported for this resource, use SHOW METHODS to view available operations for the resource and then invoke a supported method using the EXEC command")
}

func (gp *GenericProvider) GetObjectSchema(serviceName string, resourceName string, schemaName string) (*openapistackql.Schema, error) {
	svc, err := gp.GetServiceShard(serviceName, resourceName, gp.runtimeCtx)
	if err != nil {
		return nil, err
	}
	return svc.GetSchema(schemaName)
}

func (gp *GenericProvider) ShowAuth(authCtx *dto.AuthCtx) (*openapistackql.AuthMetadata, error) {
	var err error
	var retVal *openapistackql.AuthMetadata
	var authObj openapistackql.AuthMetadata
	if authCtx == nil {
		return nil, errors.New(constants.NotAuthenticatedShowStr)
	}
	switch gp.inferAuthType(*authCtx, authCtx.Type) {
	case dto.AuthServiceAccountStr:
		var sa serviceAccount
		sa, err = parseServiceAccountFile(authCtx)
		if err == nil {
			authObj = openapistackql.AuthMetadata{
				Principal: sa.Email,
				Type:      strings.ToUpper(dto.AuthServiceAccountStr),
				Source:    authCtx.GetCredentialsSourceDescriptorString(),
			}
			retVal = &authObj
			activateAuth(authCtx, sa.Email, dto.AuthServiceAccountStr)
		}
	case dto.AuthInteractiveStr:
		principal, sdkErr := sdk.GetCurrentAuthUser()
		if sdkErr == nil {
			principalStr := string(principal)
			if principalStr != "" {
				authObj = openapistackql.AuthMetadata{
					Principal: principalStr,
					Type:      strings.ToUpper(dto.AuthInteractiveStr),
					Source:    "OAuth",
				}
				retVal = &authObj
				activateAuth(authCtx, principalStr, dto.AuthInteractiveStr)
			} else {
				err = errors.New(constants.NotAuthenticatedShowStr)
			}
		} else {
			log.Infoln(sdkErr)
			err = errors.New(constants.NotAuthenticatedShowStr)
		}
	default:
		err = errors.New(constants.NotAuthenticatedShowStr)
	}
	return retVal, err
}

func (gp *GenericProvider) oAuth(authCtx *dto.AuthCtx, enforceRevokeFirst bool) (*http.Client, error) {
	var err error
	var tokenBytes []byte
	tokenBytes, err = sdk.GetAccessToken()
	if enforceRevokeFirst && authCtx.Type == dto.AuthInteractiveStr && err == nil {
		return nil, fmt.Errorf(constants.OAuthInteractiveAuthErrStr)
	}
	if err != nil {
		err = sdk.OAuthToGoogle()
		if err == nil {
			tokenBytes, err = sdk.GetAccessToken()
		}
	}
	if err != nil {
		return nil, err
	}
	activateAuth(authCtx, "", dto.AuthInteractiveStr)
	client := netutils.GetHttpClient(gp.runtimeCtx, nil)
	client.Transport = &transport{
		token:               tokenBytes,
		authType:            "Bearer",
		underlyingTransport: client.Transport,
	}
	return client, nil
}

func (gp *GenericProvider) keyFileAuth(authCtx *dto.AuthCtx) (*http.Client, error) {
	scopes := authCtx.Scopes
	if scopes == nil {
		scopes = []string{
			"https://www.googleapis.com/auth/cloud-platform",
		}
	}
	return oauthServiceAccount(authCtx, scopes, gp.runtimeCtx)
}

func (gp *GenericProvider) apiTokenFileAuth(authCtx *dto.AuthCtx) (*http.Client, error) {
	return apiTokenAuth(authCtx, gp.runtimeCtx)
}

func (gp *GenericProvider) getServiceType(service *openapistackql.Service) string {
	specialServiceNamesMap := map[string]bool{
		"storage": true,
		"compute": true,
		"dns":     true,
		"sql":     true,
	}
	nameIsSpecial, ok := specialServiceNamesMap[service.GetName()]
	cloudRegex := regexp.MustCompile(`(^https://.*cloud\.google\.com|^https://firebase\.google\.com)`)
	if service.IsPreferred() && (cloudRegex.MatchString(service.Info.Contact.URL) || (ok && nameIsSpecial)) {
		return "cloud"
	}
	return "developer"
}

func (gp *GenericProvider) GetLikeableColumns(tableName string) []string {
	var retVal []string
	switch tableName {
	case "SERVICES":
		return []string{
			"id",
			"name",
		}
	case "RESOURCES":
		return []string{
			"id",
			"name",
		}
	case "METHODS":
		return []string{
			"id",
			"name",
		}
	case "PROVIDERS":
		return []string{
			"name",
		}
	}
	return retVal
}

func (gp *GenericProvider) EnhanceMetadataFilter(metadataType string, metadataFilter func(openapistackql.ITable) (openapistackql.ITable, error), colsVisited map[string]bool) (func(openapistackql.ITable) (openapistackql.ITable, error), error) {
	typeVisited, typeOk := colsVisited["type"]
	preferredVisited, preferredOk := colsVisited["preferred"]
	sqlTrue, sqlTrueErr := sqltypeutil.InterfaceToSQLType(true)
	sqlCloudStr, sqlCloudStrErr := sqltypeutil.InterfaceToSQLType("cloud")
	equalsOperator, operatorErr := relational.GetOperatorPredicate("=")
	if sqlTrueErr != nil || sqlCloudStrErr != nil || operatorErr != nil {
		return nil, fmt.Errorf("typing and operator system broken!!!")
	}
	switch metadataType {
	case "service":
		if typeOk && typeVisited && preferredOk && preferredVisited {
			return metadataFilter, nil
		}
		if typeOk && typeVisited {
			return relational.AndTableFilters(
				metadataFilter,
				relational.ConstructTablePredicateFilter("preferred", sqlTrue, equalsOperator),
			), nil
		}
		if preferredOk && preferredVisited {
			return relational.AndTableFilters(
				metadataFilter,
				relational.ConstructTablePredicateFilter("type", sqlCloudStr, equalsOperator),
			), nil
		}
		return relational.AndTableFilters(
			relational.AndTableFilters(
				metadataFilter,
				relational.ConstructTablePredicateFilter("cloud", sqlCloudStr, equalsOperator),
			),
			relational.ConstructTablePredicateFilter("preferred", sqlTrue, equalsOperator),
		), nil
	}
	return metadataFilter, nil
}

func (gp *GenericProvider) getProviderServices() (map[string]*openapistackql.ProviderService, error) {
	retVal := make(map[string]*openapistackql.ProviderService)
	disDoc, err := gp.discoveryAdapter.GetServiceHandlesMap(gp.provider)
	if err != nil {
		return nil, err
	}
	for k, item := range disDoc {
		retVal[docparser.TranslateServiceKeyGenericProviderToIql(k)] = item
	}
	return retVal, nil
}

func (gp *GenericProvider) GetProviderServicesRedacted(runtimeCtx dto.RuntimeCtx, extended bool) (map[string]*openapistackql.ProviderService, error) {
	return gp.getProviderServices()
}

func (gp *GenericProvider) GetResourcesRedacted(currentService string, runtimeCtx dto.RuntimeCtx, extended bool) (map[string]*openapistackql.Resource, error) {
	svcDiscDocMap, err := gp.discoveryAdapter.GetResourcesMap(gp.provider, currentService)
	return svcDiscDocMap, err
}

func (gp *GenericProvider) CheckCredentialFile(authCtx *dto.AuthCtx) error {
	switch authCtx.Type {
	case dto.AuthServiceAccountStr:
		_, err := parseServiceAccountFile(authCtx)
		return err
	case dto.AuthApiKeyStr:
		_, err := authCtx.GetCredentialsBytes()
		return err
	}
	return fmt.Errorf("auth type = '%s' not supported", authCtx.Type)
}

func (gp *GenericProvider) GenerateHTTPRestInstruction(httpContext httpexec.IHttpContext) (httpexec.IHttpContext, error) {
	return httpContext, nil
}

func (gp *GenericProvider) escapeUrlParameter(k string, v string, method *openapistackql.OperationStore) string {
	if storageObjectsRegex.MatchString(method.GetName()) {
		return url.QueryEscape(v)
	}
	return v
}

func (gp *GenericProvider) Parameterise(httpContext httpexec.IHttpContext, method *openapistackql.OperationStore, parameters *dto.HttpParameters, requestSchema *openapistackql.Schema) (httpexec.IHttpContext, error) {
	visited := make(map[string]bool)
	args := make([]string, len(parameters.PathParams)*2)
	var sb strings.Builder
	var queryParams []string
	i := 0
	for k, v := range parameters.PathParams {
		if strings.Contains(httpContext.GetTemplateUrl(), "{"+k+"}") {
			args[i] = "{" + k + "}"
			args[i+1] = gp.escapeUrlParameter(k, fmt.Sprint(v), method)
			i += 2
			visited[k] = true
			continue
		}
		if strings.Contains(httpContext.GetTemplateUrl(), "{+"+k+"}") {
			args[i] = "{+" + k + "}"
			args[i+1] = gp.escapeUrlParameter(k, fmt.Sprint(v), method)
			i += 2
			visited[k] = true
			continue
		}
	}
	if len(parameters.QueryParams) > 0 {
		sb.WriteString("?")
	}
	for k, v := range parameters.QueryParams {
		vStr, vOk := v.Val.(string)
		if isVisited, kExists := visited[k]; !kExists || (!isVisited && vOk) {
			queryParams = append(queryParams, k+"="+url.QueryEscape(vStr))
			visited[k] = true
		}
	}
	sb.WriteString(strings.Join(queryParams, "&"))
	httpContext.SetUrl(strings.NewReplacer(args...).Replace(httpContext.GetTemplateUrl()) + sb.String())
	return httpContext, nil
}

func (gp *GenericProvider) SetCurrentService(serviceKey string) {
	gp.currentService = serviceKey

}

func (gp *GenericProvider) GetCurrentService() string {
	return gp.currentService
}

func (gp *GenericProvider) getPathParams(httpContext httpexec.IHttpContext) map[string]bool {
	re := regexp.MustCompile(`\{([^\{\}]+)\}`)
	keys := re.FindAllString(httpContext.GetTemplateUrl(), -1)
	retVal := make(map[string]bool, len(keys))
	for _, k := range keys {
		retVal[strings.Trim(k, "{}")] = true
	}
	return retVal
}

func (gp *GenericProvider) GetResourcesMap(serviceKey string, runtimeCtx dto.RuntimeCtx) (map[string]*openapistackql.Resource, error) {
	return gp.discoveryAdapter.GetResourcesMap(gp.provider, serviceKey)
}

func (gp *GenericProvider) GetResource(serviceKey string, resourceKey string, runtimeCtx dto.RuntimeCtx) (*openapistackql.Resource, error) {
	svc, err := gp.GetServiceShard(serviceKey, resourceKey, runtimeCtx)
	if err != nil {
		return nil, err
	}
	return svc.GetResource(resourceKey)
}

func (gp *GenericProvider) GetProviderString() string {
	return gp.provider.Name
}

func (gp *GenericProvider) GetProvider() (*openapistackql.Provider, error) {
	if gp.provider == nil {
		return nil, fmt.Errorf("nil provider object")
	}
	return gp.provider, nil
}

func (gp *GenericProvider) InferMaxResultsElement(*openapistackql.OperationStore) *dto.HTTPElement {
	return &dto.HTTPElement{
		Type: dto.QueryParam,
		Name: "maxResults",
	}
}

func (gp *GenericProvider) InferNextPageRequestElement(*openapistackql.OperationStore) *dto.HTTPElement {
	return &dto.HTTPElement{
		Type: dto.QueryParam,
		Name: "pageToken",
	}
}

func (gp *GenericProvider) InferNextPageResponseElement(*openapistackql.OperationStore) *dto.HTTPElement {
	return &dto.HTTPElement{
		Type: dto.BodyAttribute,
		Name: "nextPageToken",
	}
}
