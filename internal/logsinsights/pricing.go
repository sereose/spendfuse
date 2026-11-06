package logsinsights

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// PricingAPI is the subset of the Pricing client we use.
type PricingAPI interface {
	GetProducts(ctx context.Context, in *pricing.GetProductsInput, opts ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

// Price for Logs Insights scanning in one region.
type Price struct {
	Region string
	PerGB  float64
	Source string
}

// Fallback values from the public price list (Nov 2026). Logs Insights is
// priced per GB scanned; most regions are $0.005–0.0076.
var fallback = map[string]float64{
	"us-east-1": 0.005, "us-east-2": 0.005, "us-west-2": 0.005, "us-west-1": 0.0057,
	"eu-west-1": 0.0057, "eu-west-2": 0.0057, "eu-central-1": 0.0063, "eu-north-1": 0.005,
	"ap-southeast-1": 0.0063, "ap-southeast-2": 0.0063, "ap-northeast-1": 0.0076, "ap-south-1": 0.0057,
	"ca-central-1": 0.0057, "sa-east-1": 0.0076,
}

// LookupPrice finds the DataScanned-Bytes rate for a region; falls back silently.
func LookupPrice(ctx context.Context, api PricingAPI, region string) Price {
	fb := Price{Region: region, PerGB: 0.005, Source: "fallback"}
	if v, ok := fallback[region]; ok {
		fb.PerGB = v
	}
	if api == nil {
		return fb
	}
	var next *string
	for page := 0; page < 10; page++ {
		out, err := api.GetProducts(ctx, &pricing.GetProductsInput{
			ServiceCode: aws.String("AmazonCloudWatch"),
			Filters: []pricingtypes.Filter{
				{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("regionCode"), Value: aws.String(region)},
			},
			MaxResults: aws.Int32(100),
			NextToken:  next,
		})
		if err != nil || out == nil {
			return fb
		}
		if p, ok := parsePriceList(out.PriceList, region); ok {
			return p
		}
		if out.NextToken == nil {
			break
		}
		next = out.NextToken
	}
	return fb
}

func parsePriceList(items []string, region string) (Price, bool) {
	for _, raw := range items {
		var doc struct {
			Product struct {
				Attributes map[string]string `json:"attributes"`
			} `json:"product"`
			Terms struct {
				OnDemand map[string]struct {
					PriceDimensions map[string]struct {
						PricePerUnit map[string]string `json:"pricePerUnit"`
					} `json:"priceDimensions"`
				} `json:"OnDemand"`
			} `json:"terms"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			continue
		}
		if !strings.HasSuffix(doc.Product.Attributes["usagetype"], "DataScanned-Bytes") {
			continue
		}
		for _, term := range doc.Terms.OnDemand {
			for _, dim := range term.PriceDimensions {
				if usd, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64); err == nil && usd > 0 {
					return Price{Region: region, PerGB: usd, Source: "pricing-api"}, true
				}
			}
		}
	}
	return Price{}, false
}
