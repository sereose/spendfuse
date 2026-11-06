package logsinsights

import (
	"strconv"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/costexplorer"
	cetypes "github.com/aws/aws-sdk-go-v2/service/costexplorer/types"
)

type (
	costexplorerInput   = costexplorer.GetCostAndUsageInput
	costexplorerOutput  = costexplorer.GetCostAndUsageOutput
	costexplorerOptions = costexplorer.Options
)

// ceFake bills gbPerDay of DataScanned-Bytes at $0.005 plus an unrelated usage type.
func ceFake(gbPerDay float64, in *costexplorerInput) (*costexplorerOutput, error) {
	start, _ := time.Parse("2006-01-02", *in.TimePeriod.Start)
	end, _ := time.Parse("2006-01-02", *in.TimePeriod.End)
	var out costexplorerOutput
	for d := start; d.Before(end); d = d.AddDate(0, 0, 1) {
		out.ResultsByTime = append(out.ResultsByTime, cetypes.ResultByTime{
			TimePeriod: &cetypes.DateInterval{Start: aws.String(d.Format("2006-01-02"))},
			Groups: []cetypes.Group{
				{Keys: []string{"EUC1-DataScanned-Bytes"}, Metrics: map[string]cetypes.MetricValue{
					"UnblendedCost": {Amount: aws.String(strconv.FormatFloat(gbPerDay*0.005, 'f', -1, 64))},
					"UsageQuantity": {Amount: aws.String(strconv.FormatFloat(gbPerDay, 'f', -1, 64))},
				}},
				{Keys: []string{"EUC1-DataProcessing-Bytes"}, Metrics: map[string]cetypes.MetricValue{
					"UnblendedCost": {Amount: aws.String("12.34")},
					"UsageQuantity": {Amount: aws.String("21.6")},
				}},
			},
		})
	}
	return &out, nil
}
