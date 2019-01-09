package providers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"sync"

	lru "github.com/hashicorp/golang-lru"
	"golang.org/x/oauth2/clientcredentials"
)

// azureGroupCacheSize controls the size of the caches of AD group info
const azureGroupCacheSize = 1024

// GraphService wraps calls to provider admin APIs
type GraphService interface {
	GetGroups(string) ([]string, error)
}

// AzureGraphService implements graph API calls for the Azure provider
type AzureGraphService struct {
	client               *http.Client
	groupMembershipCache *lru.Cache
	groupNameCache       *lru.Cache
}

// NewAzureGraphService creates a new graph service for getting groups
func NewAzureGraphService(clientID string, clientSecret string, tokenURL string) *AzureGraphService {
	clientConfig := &clientcredentials.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		TokenURL:     tokenURL,
		Scopes: []string{
			"https://graph.microsoft.com/.default",
		},
	}
	ctx := context.Background()
	client := clientConfig.Client(ctx)
	memberCache, err := lru.New(azureGroupCacheSize)
	if err != nil {
		panic(err) // Should only happen if azureGroupCacheSize is a negative number
	}
	nameCache, err := lru.New(azureGroupCacheSize)
	if err != nil {
		panic(err) // Should only happen if azureGroupCacheSize is a negative number
	}
	return &AzureGraphService{
		client:               client,
		groupMembershipCache: memberCache,
		groupNameCache:       nameCache,
	}
}

// GetGroups lists groups user belongs to.
func (gs *AzureGraphService) GetGroups(email string) ([]string, error) {
	if gs.client == nil {
		return nil, errors.New("oauth client must be configured")
	}
	if email == "" {
		return nil, errors.New("missing email")
	}

	var wg sync.WaitGroup
	var mux sync.Mutex
	var err error
	groupNames := make([]string, 0)
	// See: https://developer.microsoft.com/en-us/graph/docs/api-reference/beta/api/user_getmembergroups
	requestBody := `{"securityEnabledOnly": false}`
	requestURL := fmt.Sprintf("https://graph.microsoft.com/beta/users/%s/getMemberGroups", url.PathEscape(email))
	for {
		groupResponse, err := gs.client.Post(requestURL, "application/json", strings.NewReader(requestBody))
		if err != nil {
			return nil, err
		}

		groupData := struct {
			// Link to next page of data, see:
			// https://docs.microsoft.com/en-us/graph/query-parameters#skip-parameter
			Next  string   `json:"@odata.nextLink"`
			Value []string `json:"value"`
		}{}

		body, err := ioutil.ReadAll(groupResponse.Body)
		if err != nil {
			return nil, err
		}
		if groupResponse.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("api error: %s", string(body))
		}

		err = json.Unmarshal(body, &groupData)
		if err != nil {
			return nil, err
		}

		for _, groupID := range groupData.Value {
			wg.Add(1)
			id := groupID
			go func(wg *sync.WaitGroup) {
				defer wg.Done()

				var name string
				// check the cache for the group name first
				if cachedName, ok := gs.groupNameCache.Get(id); !ok {
					// didn't have the group name, make concurrent API call to fetch it
					name, err = gs.getGroupName(id)
					if err == nil {
						// got the name ok, populate the cache
						gs.groupNameCache.Add(id, name)
					}
				} else {
					// cache hit
					name = cachedName.(string)
				}
				mux.Lock()
				groupNames = append(groupNames, name)
				mux.Unlock()
			}(&wg)
		}

		if groupData.Next != "" {
			requestURL = groupData.Next
		} else {
			break
		}
	}
	wg.Wait()
	if err != nil {
		return nil, err
	}

	return groupNames, nil
}

// getGroupName returns the group name, preferentially pulling from cache
func (gs *AzureGraphService) getGroupName(id string) (string, error) {
	if gs.client == nil {
		return "", errors.New("oauth client must be configured")
	}
	// See: https://developer.microsoft.com/en-us/graph/docs/api-reference/v1.0/api/group_get
	requestURL := fmt.Sprintf("https://graph.microsoft.com/v1.0/groups/%s", url.PathEscape(id))
	groupMetaResponse, err := gs.client.Get(requestURL)
	if err != nil {
		return "", err
	}

	groupMetadata := struct {
		DisplayName string `json:"displayName"`
	}{}

	body, err := ioutil.ReadAll(groupMetaResponse.Body)
	if err != nil {
		return "", err
	}
	if groupMetaResponse.StatusCode != http.StatusOK {
		return "", fmt.Errorf("api error: %s", string(body))
	}

	err = json.Unmarshal(body, &groupMetadata)
	if err != nil {
		return "", err
	}

	return groupMetadata.DisplayName, nil
}
