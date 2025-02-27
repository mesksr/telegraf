package elasticsearch

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"text/template"
	"time"

	"crypto/sha256"

	"github.com/olivere/elastic"

	"github.com/influxdata/telegraf"
	"github.com/influxdata/telegraf/config"
	"github.com/influxdata/telegraf/plugins/common/tls"
	"github.com/influxdata/telegraf/plugins/outputs"
)

type Elasticsearch struct {
	AuthBearerToken     string          `toml:"auth_bearer_token"`
	DefaultPipeline     string          `toml:"default_pipeline"`
	DefaultTagValue     string          `toml:"default_tag_value"`
	EnableGzip          bool            `toml:"enable_gzip"`
	EnableSniffer       bool            `toml:"enable_sniffer"`
	FloatHandling       string          `toml:"float_handling"`
	FloatReplacement    float64         `toml:"float_replacement_value"`
	ForceDocumentID     bool            `toml:"force_document_id"`
	HealthCheckInterval config.Duration `toml:"health_check_interval"`
	HealthCheckTimeout  config.Duration `toml:"health_check_timeout"`
	IndexName           string          `toml:"index_name"`
	ManageTemplate      bool            `toml:"manage_template"`
	OverwriteTemplate   bool            `toml:"overwrite_template"`
	Password            string          `toml:"password"`
	TemplateName        string          `toml:"template_name"`
	Timeout             config.Duration `toml:"timeout"`
	URLs                []string        `toml:"urls"`
	UsePipeline         string          `toml:"use_pipeline"`
	Username            string          `toml:"username"`
	Log                 telegraf.Logger `toml:"-"`
	majorReleaseNumber  int
	pipelineName        string
	pipelineTagKeys     []string
	tagKeys             []string
	tls.ClientConfig

	Client *elastic.Client
}

var sampleConfig = `
  ## The full HTTP endpoint URL for your Elasticsearch instance
  ## Multiple urls can be specified as part of the same cluster,
  ## this means that only ONE of the urls will be written to each interval.
  urls = [ "http://node1.es.example.com:9200" ] # required.
  ## Elasticsearch client timeout, defaults to "5s" if not set.
  timeout = "5s"
  ## Set to true to ask Elasticsearch a list of all cluster nodes,
  ## thus it is not necessary to list all nodes in the urls config option.
  enable_sniffer = false
  ## Set to true to enable gzip compression
  enable_gzip = false
  ## Set the interval to check if the Elasticsearch nodes are available
  ## Setting to "0s" will disable the health check (not recommended in production)
  health_check_interval = "10s"
  ## Set the timeout for periodic health checks.
  # health_check_timeout = "1s"
  ## HTTP basic authentication details
  # username = "telegraf"
  # password = "mypassword"
  ## HTTP bearer token authentication details
  # auth_bearer_token = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9"

  ## Index Config
  ## The target index for metrics (Elasticsearch will create if it not exists).
  ## You can use the date specifiers below to create indexes per time frame.
  ## The metric timestamp will be used to decide the destination index name
  # %Y - year (2016)
  # %y - last two digits of year (00..99)
  # %m - month (01..12)
  # %d - day of month (e.g., 01)
  # %H - hour (00..23)
  # %V - week of the year (ISO week) (01..53)
  ## Additionally, you can specify a tag name using the notation {{tag_name}}
  ## which will be used as part of the index name. If the tag does not exist,
  ## the default tag value will be used.
  # index_name = "telegraf-{{host}}-%Y.%m.%d"
  # default_tag_value = "none"
  index_name = "telegraf-%Y.%m.%d" # required.

  ## Optional TLS Config
  # tls_ca = "/etc/telegraf/ca.pem"
  # tls_cert = "/etc/telegraf/cert.pem"
  # tls_key = "/etc/telegraf/key.pem"
  ## Use TLS but skip chain & host verification
  # insecure_skip_verify = false

  ## Template Config
  ## Set to true if you want telegraf to manage its index template.
  ## If enabled it will create a recommended index template for telegraf indexes
  manage_template = true
  ## The template name used for telegraf indexes
  template_name = "telegraf"
  ## Set to true if you want telegraf to overwrite an existing template
  overwrite_template = false
  ## If set to true a unique ID hash will be sent as sha256(concat(timestamp,measurement,series-hash)) string
  ## it will enable data resend and update metric points avoiding duplicated metrics with diferent id's
  force_document_id = false

  ## Specifies the handling of NaN and Inf values.
  ## This option can have the following values:
  ##    none    -- do not modify field-values (default); will produce an error if NaNs or infs are encountered
  ##    drop    -- drop fields containing NaNs or infs
  ##    replace -- replace with the value in "float_replacement_value" (default: 0.0)
  ##               NaNs and inf will be replaced with the given number, -inf with the negative of that number
  # float_handling = "none"
  # float_replacement_value = 0.0

  ## Pipeline Config
  ## To use a ingest pipeline, set this to the name of the pipeline you want to use.
  # use_pipeline = "my_pipeline"
  ## Additionally, you can specify a tag name using the notation {{tag_name}}
  ## which will be used as part of the pipeline name. If the tag does not exist,
  ## the default pipeline will be used as the pipeline. If no default pipeline is set,
  ## no pipeline is used for the metric.
  # use_pipeline = "{{es_pipeline}}"
  # default_pipeline = "my_pipeline"
`

const telegrafTemplate = `
{
	{{ if (lt .Version 6) }}
	"template": "{{.TemplatePattern}}",
	{{ else }}
	"index_patterns" : [ "{{.TemplatePattern}}" ],
	{{ end }}
	"settings": {
		"index": {
			"refresh_interval": "10s",
			"mapping.total_fields.limit": 5000,
			"auto_expand_replicas" : "0-1",
			"codec" : "best_compression"
		}
	},
	"mappings" : {
		{{ if (lt .Version 7) }}
		"metrics" : {
			{{ if (lt .Version 6) }}
			"_all": { "enabled": false },
			{{ end }}
		{{ end }}
		"properties" : {
			"@timestamp" : { "type" : "date" },
			"measurement_name" : { "type" : "keyword" }
		},
		"dynamic_templates": [
			{
				"tags": {
					"match_mapping_type": "string",
					"path_match": "tag.*",
					"mapping": {
						"ignore_above": 512,
						"type": "keyword"
					}
				}
			},
			{
				"metrics_long": {
					"match_mapping_type": "long",
					"mapping": {
						"type": "float",
						"index": false
					}
				}
			},
			{
				"metrics_double": {
					"match_mapping_type": "double",
					"mapping": {
						"type": "float",
						"index": false
					}
				}
			},
			{
				"text_fields": {
					"match": "*",
					"mapping": {
						"norms": false
					}
				}
			}
		]
		{{ if (lt .Version 7) }}
		}
		{{ end }}
	}
}`

type templatePart struct {
	TemplatePattern string
	Version         int
}

func (a *Elasticsearch) Connect() error {
	if a.URLs == nil || a.IndexName == "" {
		return fmt.Errorf("elasticsearch urls or index_name is not defined")
	}

	// Determine if we should process NaN and inf values
	switch a.FloatHandling {
	case "", "none":
		a.FloatHandling = "none"
	case "drop", "replace":
	default:
		return fmt.Errorf("invalid float_handling type %q", a.FloatHandling)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()

	var clientOptions []elastic.ClientOptionFunc

	tlsCfg, err := a.ClientConfig.TLSConfig()
	if err != nil {
		return err
	}
	tr := &http.Transport{
		TLSClientConfig: tlsCfg,
	}

	httpclient := &http.Client{
		Transport: tr,
		Timeout:   time.Duration(a.Timeout),
	}

	elasticURL, err := url.Parse(a.URLs[0])
	if err != nil {
		return fmt.Errorf("parsing URL failed: %v", err)
	}

	clientOptions = append(clientOptions,
		elastic.SetHttpClient(httpclient),
		elastic.SetSniff(a.EnableSniffer),
		elastic.SetScheme(elasticURL.Scheme),
		elastic.SetURL(a.URLs...),
		elastic.SetHealthcheckInterval(time.Duration(a.HealthCheckInterval)),
		elastic.SetHealthcheckTimeout(time.Duration(a.HealthCheckTimeout)),
		elastic.SetGzip(a.EnableGzip),
	)

	if a.Username != "" && a.Password != "" {
		clientOptions = append(clientOptions,
			elastic.SetBasicAuth(a.Username, a.Password),
		)
	}

	if a.AuthBearerToken != "" {
		clientOptions = append(clientOptions,
			elastic.SetHeaders(http.Header{
				"Authorization": []string{fmt.Sprintf("Bearer %s", a.AuthBearerToken)},
			}),
		)
	}

	if time.Duration(a.HealthCheckInterval) == 0 {
		clientOptions = append(clientOptions,
			elastic.SetHealthcheck(false),
		)
		a.Log.Debugf("Disabling health check")
	}

	client, err := elastic.NewClient(clientOptions...)

	if err != nil {
		return err
	}

	// check for ES version on first node
	esVersion, err := client.ElasticsearchVersion(a.URLs[0])

	if err != nil {
		return fmt.Errorf("elasticsearch version check failed: %s", err)
	}

	// quit if ES version is not supported
	majorReleaseNumber, err := strconv.Atoi(strings.Split(esVersion, ".")[0])
	if err != nil || majorReleaseNumber < 5 {
		return fmt.Errorf("elasticsearch version not supported: %s", esVersion)
	}

	a.Log.Infof("Elasticsearch version: %q", esVersion)

	a.Client = client
	a.majorReleaseNumber = majorReleaseNumber

	if a.ManageTemplate {
		err := a.manageTemplate(ctx)
		if err != nil {
			return err
		}
	}

	a.IndexName, a.tagKeys = a.GetTagKeys(a.IndexName)
	a.pipelineName, a.pipelineTagKeys = a.GetTagKeys(a.UsePipeline)

	return nil
}

// GetPointID generates a unique ID for a Metric Point
func GetPointID(m telegraf.Metric) string {
	var buffer bytes.Buffer
	//Timestamp(ns),measurement name and Series Hash for compute the final SHA256 based hash ID

	buffer.WriteString(strconv.FormatInt(m.Time().Local().UnixNano(), 10)) //nolint:revive // from buffer.go: "err is always nil"
	buffer.WriteString(m.Name())                                           //nolint:revive // from buffer.go: "err is always nil"
	buffer.WriteString(strconv.FormatUint(m.HashID(), 10))                 //nolint:revive // from buffer.go: "err is always nil"

	return fmt.Sprintf("%x", sha256.Sum256(buffer.Bytes()))
}

func (a *Elasticsearch) Write(metrics []telegraf.Metric) error {
	if len(metrics) == 0 {
		return nil
	}

	bulkRequest := a.Client.Bulk()

	for _, metric := range metrics {
		var name = metric.Name()

		// index name has to be re-evaluated each time for telegraf
		// to send the metric to the correct time-based index
		indexName := a.GetIndexName(a.IndexName, metric.Time(), a.tagKeys, metric.Tags())

		// Handle NaN and inf field-values
		fields := make(map[string]interface{})
		for k, value := range metric.Fields() {
			v, ok := value.(float64)
			if !ok || a.FloatHandling == "none" || !(math.IsNaN(v) || math.IsInf(v, 0)) {
				fields[k] = value
				continue
			}
			if a.FloatHandling == "drop" {
				continue
			}

			if math.IsNaN(v) || math.IsInf(v, 1) {
				fields[k] = a.FloatReplacement
			} else {
				fields[k] = -a.FloatReplacement
			}
		}

		m := make(map[string]interface{})

		m["@timestamp"] = metric.Time()
		m["measurement_name"] = name
		m["tag"] = metric.Tags()
		m[name] = fields

		br := elastic.NewBulkIndexRequest().Index(indexName).Doc(m)

		if a.ForceDocumentID {
			id := GetPointID(metric)
			br.Id(id)
		}

		if a.majorReleaseNumber <= 6 {
			br.Type("metrics")
		}

		if a.UsePipeline != "" {
			if pipelineName := a.getPipelineName(a.pipelineName, a.pipelineTagKeys, metric.Tags()); pipelineName != "" {
				br.Pipeline(pipelineName)
			}
		}

		bulkRequest.Add(br)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(a.Timeout))
	defer cancel()

	res, err := bulkRequest.Do(ctx)

	if err != nil {
		return fmt.Errorf("error sending bulk request to Elasticsearch: %s", err)
	}

	if res.Errors {
		for id, err := range res.Failed() {
			a.Log.Errorf("Elasticsearch indexing failure, id: %d, error: %s, caused by: %s, %s", id, err.Error.Reason, err.Error.CausedBy["reason"], err.Error.CausedBy["type"])
			break
		}
		return fmt.Errorf("elasticsearch failed to index %d metrics", len(res.Failed()))
	}

	return nil
}

func (a *Elasticsearch) manageTemplate(ctx context.Context) error {
	if a.TemplateName == "" {
		return fmt.Errorf("elasticsearch template_name configuration not defined")
	}

	templateExists, errExists := a.Client.IndexTemplateExists(a.TemplateName).Do(ctx)

	if errExists != nil {
		return fmt.Errorf("elasticsearch template check failed, template name: %s, error: %s", a.TemplateName, errExists)
	}

	templatePattern := a.IndexName

	if strings.Contains(templatePattern, "%") {
		templatePattern = templatePattern[0:strings.Index(templatePattern, "%")]
	}

	if strings.Contains(templatePattern, "{{") {
		templatePattern = templatePattern[0:strings.Index(templatePattern, "{{")]
	}

	if templatePattern == "" {
		return fmt.Errorf("template cannot be created for dynamic index names without an index prefix")
	}

	if (a.OverwriteTemplate) || (!templateExists) || (templatePattern != "") {
		tp := templatePart{
			TemplatePattern: templatePattern + "*",
			Version:         a.majorReleaseNumber,
		}

		t := template.Must(template.New("template").Parse(telegrafTemplate))
		var tmpl bytes.Buffer

		if err := t.Execute(&tmpl, tp); err != nil {
			return err
		}
		_, errCreateTemplate := a.Client.IndexPutTemplate(a.TemplateName).BodyString(tmpl.String()).Do(ctx)

		if errCreateTemplate != nil {
			return fmt.Errorf("elasticsearch failed to create index template %s : %s", a.TemplateName, errCreateTemplate)
		}

		a.Log.Debugf("Template %s created or updated\n", a.TemplateName)
	} else {
		a.Log.Debug("Found existing Elasticsearch template. Skipping template management")
	}
	return nil
}

func (a *Elasticsearch) GetTagKeys(indexName string) (string, []string) {
	tagKeys := []string{}
	startTag := strings.Index(indexName, "{{")

	for startTag >= 0 {
		endTag := strings.Index(indexName, "}}")

		if endTag < 0 {
			startTag = -1
		} else {
			tagName := indexName[startTag+2 : endTag]

			var tagReplacer = strings.NewReplacer(
				"{{"+tagName+"}}", "%s",
			)

			indexName = tagReplacer.Replace(indexName)
			tagKeys = append(tagKeys, strings.TrimSpace(tagName))

			startTag = strings.Index(indexName, "{{")
		}
	}

	return indexName, tagKeys
}

func (a *Elasticsearch) GetIndexName(indexName string, eventTime time.Time, tagKeys []string, metricTags map[string]string) string {
	if strings.Contains(indexName, "%") {
		var dateReplacer = strings.NewReplacer(
			"%Y", eventTime.UTC().Format("2006"),
			"%y", eventTime.UTC().Format("06"),
			"%m", eventTime.UTC().Format("01"),
			"%d", eventTime.UTC().Format("02"),
			"%H", eventTime.UTC().Format("15"),
			"%V", getISOWeek(eventTime.UTC()),
		)

		indexName = dateReplacer.Replace(indexName)
	}

	tagValues := []interface{}{}

	for _, key := range tagKeys {
		if value, ok := metricTags[key]; ok {
			tagValues = append(tagValues, value)
		} else {
			a.Log.Debugf("Tag '%s' not found, using '%s' on index name instead\n", key, a.DefaultTagValue)
			tagValues = append(tagValues, a.DefaultTagValue)
		}
	}

	return fmt.Sprintf(indexName, tagValues...)
}

func (a *Elasticsearch) getPipelineName(pipelineInput string, tagKeys []string, metricTags map[string]string) string {
	if !strings.Contains(pipelineInput, "%") || len(tagKeys) == 0 {
		return pipelineInput
	}

	var tagValues []interface{}

	for _, key := range tagKeys {
		if value, ok := metricTags[key]; ok {
			tagValues = append(tagValues, value)
			continue
		}
		a.Log.Debugf("Tag %s not found, reverting to default pipeline instead.", key)
		return a.DefaultPipeline
	}
	return fmt.Sprintf(pipelineInput, tagValues...)
}

func getISOWeek(eventTime time.Time) string {
	_, week := eventTime.ISOWeek()
	return strconv.Itoa(week)
}

func (a *Elasticsearch) SampleConfig() string {
	return sampleConfig
}

func (a *Elasticsearch) Description() string {
	return "Configuration for Elasticsearch to send metrics to."
}

func (a *Elasticsearch) Close() error {
	a.Client = nil
	return nil
}

func init() {
	outputs.Add("elasticsearch", func() telegraf.Output {
		return &Elasticsearch{
			Timeout:             config.Duration(time.Second * 5),
			HealthCheckInterval: config.Duration(time.Second * 10),
			HealthCheckTimeout:  config.Duration(time.Second * 1),
		}
	})
}
