package eval

import (
	"encoding/json"
	"net/http"
	"strconv"
	"sync"
	"time"
)

var openRouterModelsURL = "https://openrouter.ai/api/v1/models"

var (
	openRouterPricingOnce = &sync.Once{}
	openRouterPricing     map[string]Price
)

// resetOpenRouterPricing replaces the fetch guard and cache for tests.
func resetOpenRouterPricing(url string) {
	openRouterModelsURL = url
	openRouterPricingOnce = &sync.Once{}
	openRouterPricing = nil
}

type openRouterModelsResponse struct {
	Data []struct {
		ID      string `json:"id"`
		Pricing struct {
			Prompt         string `json:"prompt"`
			Completion     string `json:"completion"`
			InputCacheRead string `json:"input_cache_read"`
		} `json:"pricing"`
	} `json:"data"`
}

func loadOpenRouterPricing() {
	openRouterPricingOnce.Do(func() {
		if openRouterModelsURL == "" {
			return
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(openRouterModelsURL)
		if err != nil {
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return
		}
		var catalog openRouterModelsResponse
		if err := json.NewDecoder(resp.Body).Decode(&catalog); err != nil {
			return
		}
		prices := make(map[string]Price, len(catalog.Data))
		for _, model := range catalog.Data {
			price := Price{
				InPerM:        parseOpenRouterPrice(model.Pricing.Prompt),
				OutPerM:       parseOpenRouterPrice(model.Pricing.Completion),
				CacheReadPerM: parseOpenRouterPrice(model.Pricing.InputCacheRead),
			}
			if price.InPerM != 0 || price.OutPerM != 0 {
				prices[model.ID] = price
			}
		}
		openRouterPricing = prices
	})
}

func parseOpenRouterPrice(value string) float64 {
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0
	}
	return parsed * 1e6
}

func openRouterPrice(model string) (Price, bool) {
	loadOpenRouterPricing()
	price, ok := openRouterPricing[model]
	return price, ok
}
