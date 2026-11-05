package nat

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/pricing"
	pricingtypes "github.com/aws/aws-sdk-go-v2/service/pricing/types"
)

// Price is what a NAT Gateway costs in one region.
type Price struct {
	Region  string
	PerHour float64 // USD per gateway-hour
	PerGB   float64 // USD per GB processed (both directions through the gateway)
	Source  string  // "pricing-api" or "fallback"
}

// fallbackPrices covers the regions most prospects run in. Values from the
// public price list, Nov 2026; used only when the Pricing API is unreachable
// or returns nothing. Both numbers are identical per region by AWS design.
var fallbackPrices = map[string]float64{
	"us-east-1": 0.045, "us-east-2": 0.045, "us-west-1": 0.048, "us-west-2": 0.045,
	"eu-west-1": 0.048, "eu-west-2": 0.048, "eu-west-3": 0.05, "eu-central-1": 0.052,
	"eu-north-1": 0.044, "eu-south-1": 0.048,
	"ap-south-1": 0.056, "ap-southeast-1": 0.059, "ap-southeast-2": 0.059,
	"ap-northeast-1": 0.062, "ap-northeast-2": 0.059,
	"ca-central-1": 0.05, "sa-east-1": 0.093,
}

// PricingAPI is the subset of the Pricing client we use.
type PricingAPI interface {
	GetProducts(ctx context.Context, in *pricing.GetProductsInput, opts ...func(*pricing.Options)) (*pricing.GetProductsOutput, error)
}

// LookupPrice asks the Pricing API for NAT Gateway on-demand prices in region.
// It never fails hard: on any problem it returns the fallback and marks Source.
func LookupPrice(ctx context.Context, api PricingAPI, region string) Price {
	fb := Price{Region: region, PerHour: 0.045, PerGB: 0.045, Source: "fallback"}
	if v, ok := fallbackPrices[region]; ok {
		fb.PerHour, fb.PerGB = v, v
	}
	if api == nil {
		return fb
	}

	out, err := api.GetProducts(ctx, &pricing.GetProductsInput{
		ServiceCode: aws.String("AmazonEC2"),
		Filters: []pricingtypes.Filter{
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("productFamily"), Value: aws.String("NAT Gateway")},
			{Type: pricingtypes.FilterTypeTermMatch, Field: aws.String("regionCode"), Value: aws.String(region)},
		},
		MaxResults: aws.Int32(20),
	})
	if err != nil || out == nil {
		return fb
	}
	p, ok := parsePriceList(out.PriceList, region)
	if !ok {
		return fb
	}
	return p
}

// parsePriceList extracts hourly and per-GB prices from Pricing API JSON blobs.
// Exported-for-test via parsePriceList in tests.
func parsePriceList(items []string, region string) (Price, bool) {
	p := Price{Region: region, Source: "pricing-api"}
	for _, raw := range items {
		var doc struct {
			Product struct {
				Attributes map[string]string `json:"attributes"`
			} `json:"product"`
			Terms struct {
				OnDemand map[string]struct {
					PriceDimensions map[string]struct {
						Unit         string            `json:"unit"`
						PricePerUnit map[string]string `json:"pricePerUnit"`
					} `json:"priceDimensions"`
				} `json:"OnDemand"`
			} `json:"terms"`
		}
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			continue
		}
		usage := doc.Product.Attributes["usagetype"]
		for _, term := range doc.Terms.OnDemand {
			for _, dim := range term.PriceDimensions {
				usd, err := strconv.ParseFloat(dim.PricePerUnit["USD"], 64)
				if err != nil || usd == 0 {
					continue
				}
				switch {
				case strings.EqualFold(dim.Unit, "Hrs") || strings.Contains(usage, "Hours"):
					p.PerHour = usd
				case strings.EqualFold(dim.Unit, "GB") || strings.Contains(usage, "Bytes"):
					p.PerGB = usd
				}
			}
		}
	}
	if p.PerHour == 0 || p.PerGB == 0 {
		return Price{}, false
	}
	return p, true
}

func (p Price) String() string {
	return fmt.Sprintf("$%.3f/h + $%.3f/GB (%s, %s)", p.PerHour, p.PerGB, p.Region, p.Source)
}
