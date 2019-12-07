package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"os/signal"
	"plugin"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"text/template"
	"time"

	"github.com/BurntSushi/toml"
	client "github.com/influxdata/influxdb1-client/v2"
	"github.com/rwynn/gtm"
	"github.com/tomochain/mongofluxd/mongofluxdplug"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"go.mongodb.org/mongo-driver/mongo/writeconcern"
)

var exitStatus = 0
var infoLog *log.Logger = log.New(os.Stdout, "INFO ", log.Flags())
var errorLog *log.Logger = log.New(os.Stdout, "ERROR ", log.Flags())

const (
	Name                  = "mongofluxd"
	Version               = "1.2.1"
	mongoUrlDefault       = "mongodb://localhost:27017"
	influxUrlDefault      = "http://localhost:8086"
	influxClientsDefault  = 10
	influxBufferDefault   = 1000
	resumeNameDefault     = "default"
	gtmChannelSizeDefault = 512
)

type resumeStrategy int

const (
	timestampResumeStrategy resumeStrategy = iota
	tokenResumeStrategy
)

func (arg *resumeStrategy) String() string {
	return fmt.Sprintf("%d", *arg)
}

func (arg *resumeStrategy) Set(value string) (err error) {
	var i int
	if i, err = strconv.Atoi(value); err != nil {
		return
	}
	rs := resumeStrategy(i)
	*arg = rs
	return
}

type gtmSettings struct {
	ChannelSize    int    `toml:"channel-size"`
	BufferSize     int    `toml:"buffer-size"`
	BufferDuration string `toml:"buffer-duration"`
}

type measureSettings struct {
	Namespace string
	View      string
	Timefield string
	Retention string
	Precision string
	Measure   string
	Database  string
	Symbol    string
	Tags      []string
	Fields    []string
	plug      func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error)
}

type configOptions struct {
	MongoURL                 string      `toml:"mongo-url"`
	MongoOpLogDatabaseName   string      `toml:"mongo-oplog-database-name"`
	MongoOpLogCollectionName string      `toml:"mongo-oplog-collection-name"`
	GtmSettings              gtmSettings `toml:"gtm-settings"`
	ResumeName               string      `toml:"resume-name"`
	Version                  bool
	Verbose                  bool
	Resume                   bool
	ResumeStrategy           resumeStrategy `toml:"resume-strategy"`
	ResumeWriteUnsafe        bool           `toml:"resume-write-unsafe"`
	ResumeFromTimestamp      int64          `toml:"resume-from-timestamp"`
	Replay                   bool
	ConfigFile               string
	Measurement              []*measureSettings
	InfluxURL                string `toml:"influx-url"`
	InfluxUser               string `toml:"influx-user"`
	InfluxPassword           string `toml:"influx-password"`
	InfluxSkipVerify         bool   `toml:"influx-skip-verify"`
	InfluxPemFile            string `toml:"influx-pem-file"`
	InfluxAutoCreateDB       bool   `toml:"influx-auto-create-db"`
	InfluxClients            int    `toml:"influx-clients"`
	InfluxBufferSize         int    `toml:"influx-buffer-size"`
	DirectReads              bool   `toml:"direct-reads"`
	ChangeStreams            bool   `toml:"change-streams"`
	ExitAfterDirectReads     bool   `toml:"exit-after-direct-reads"`
	PluginPath               string `toml:"plugin-path"`
}

type dbcol struct {
	db  string
	col string
}

type InfluxMeasure struct {
	ns         string
	view       *dbcol
	timefield  string
	retention  string
	precision  string
	measure    string
	measureTpl *template.Template
	database   string
	tags       map[string]string
	fields     map[string]string
	plug       func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error)
}

type InfluxCtx struct {
	m        map[string]client.BatchPoints
	c        client.Client
	dbs      map[string]bool
	measures map[string]*InfluxMeasure
	config   *configOptions
	lastTs   primitive.Timestamp
	client   *mongo.Client
	tokens   bson.M
}

type InfluxDataMap struct {
	op        *gtm.Op
	tags      map[string]string
	fields    map[string]interface{}
	timefield bool
	measure   *InfluxMeasure
	t         time.Time
	name      string
	nameTpl   *template.Template
}

func TimestampTime(ts primitive.Timestamp) time.Time {
	return time.Unix(int64(ts.T), 0).UTC()
}

func (im *InfluxMeasure) parseView(view string) error {
	dbCol := strings.SplitN(view, ".", 2)
	if len(dbCol) != 2 {
		return fmt.Errorf("View namespace is invalid: %s", view)
	}
	im.view = &dbcol{
		db:  dbCol[0],
		col: dbCol[1],
	}
	return nil
}

func (ctx *InfluxCtx) saveTs() (err error) {
	if ctx.config.Resume && ctx.lastTs.T > 0 {
		if err = ctx.writeBatch(); err != nil {
			return err
		}
		if ctx.config.ResumeStrategy == tokenResumeStrategy {
			err = saveTokens(ctx.client, ctx.tokens, ctx.config)
			if err == nil {
				ctx.tokens = bson.M{}
			}
		} else {
			err = saveTimestamp(ctx.client, ctx.lastTs, ctx.config)
		}
		ctx.lastTs = primitive.Timestamp{}
	}
	return
}

func (ctx *InfluxCtx) setupMeasurements() error {
	mss := ctx.config.Measurement
	if len(mss) > 0 {
		for _, ms := range mss {
			im := &InfluxMeasure{
				ns:        ms.Namespace,
				timefield: ms.Timefield,
				retention: ms.Retention,
				precision: ms.Precision,
				measure:   ms.Measure,
				database:  ms.Database,
				plug:      ms.plug,
				tags:      make(map[string]string),
				fields:    make(map[string]string),
			}
			if ms.View != "" {
				im.ns = ms.View
				if err := im.parseView(ms.View); err != nil {
					return err
				}
			}
			if im.database == "" {
				im.database = strings.SplitN(im.ns, ".", 2)[0]
			}
			if im.measure == "" {
				im.measure = strings.SplitN(im.ns, ".", 2)[1]
			} else {
				if strings.Contains(im.measure, "{{") {
					// detect and create go text/template for measure name
					tpl, err := template.New(im.ns).Parse(im.measure)
					if err != nil {
						return err
					}
					im.measureTpl = tpl
				}
			}
			if im.precision == "" {
				im.precision = "s"
			}
			for _, tag := range ms.Tags {
				names := strings.SplitN(tag, ":", 2)
				if len(names) < 2 {
					im.tags[names[0]] = names[0]
				} else {
					im.tags[names[0]] = names[1]
				}
			}
			for _, field := range ms.Fields {
				names := strings.SplitN(field, ":", 2)
				if len(names) < 2 {
					im.fields[names[0]] = names[0]
				} else {
					im.fields[names[0]] = names[1]
				}
			}
			if im.plug == nil {
				if len(im.fields) == 0 {
					return fmt.Errorf("at least one field is required per measurement")
				}
			}
			ctx.measures[ms.Namespace] = im
			if ms.View != "" {
				ctx.measures[ms.View] = im
			}
		}
		return nil
	} else {
		return fmt.Errorf("At least one measurement is required")
	}
}

func (ctx *InfluxCtx) createDatabase(db string) error {
	if ctx.config.InfluxAutoCreateDB {
		if ctx.dbs[db] == false {
			q := client.NewQuery(fmt.Sprintf(`CREATE DATABASE "%s"`, db), "", "")
			if response, err := ctx.c.Query(q); err != nil || response.Error() != nil {
				if err != nil {
					return err
				} else {
					return response.Error()
				}
			} else {
				ctx.dbs[db] = true
			}
		}
	}
	return nil
}

func (ctx *InfluxCtx) setupDatabase(op *gtm.Op) error {
	ns := op.Namespace
	if _, found := ctx.m[ns]; found == false {
		measure := ctx.measures[ns]
		bp, err := client.NewBatchPoints(client.BatchPointsConfig{
			Database:        measure.database,
			RetentionPolicy: measure.retention,
			Precision:       measure.precision,
		})
		if err != nil {
			return err
		}
		ctx.m[ns] = bp
		if err := ctx.createDatabase(measure.database); err != nil {
			return err
		}
	}
	return nil
}

func (ctx *InfluxCtx) writeBatch() (err error) {
	points := 0
	for _, bp := range ctx.m {
		points += len(bp.Points())
		if err = ctx.c.Write(bp); err != nil {
			break
		}
	}
	if ctx.config.Verbose {
		if points > 0 {
			infoLog.Printf("%d points flushed\n", points)
		}
	}
	ctx.m = make(map[string]client.BatchPoints)
	return
}

func (m *InfluxDataMap) istagtype(v interface{}) bool {
	switch v.(type) {
	case string:
		return true
	default:
		return false
	}
}

func (m *InfluxDataMap) isfieldtype(v interface{}) bool {
	switch v.(type) {
	case string:
		return true
	case int:
		return true
	case int32:
		return true
	case int64:
		return true
	case float32:
		return true
	case float64:
		return true
	case bool:
		return true
	default:
		return false
	}
}

func (m *InfluxDataMap) flatmap(prefix string, e map[string]interface{}) map[string]interface{} {
	o := make(map[string]interface{})
	for k, v := range e {
		switch child := v.(type) {
		case map[string]interface{}:
			nm := m.flatmap("", child)
			for nk, nv := range nm {
				o[prefix+k+"."+nk] = nv
			}
		default:
			if m.isfieldtype(v) {
				o[prefix+k] = v
			}
		}
	}
	return o
}

func (m *InfluxDataMap) unsupportedType(op *gtm.Op, k string, v interface{}, kind string) {
	errorLog.Printf("Unsupported type %T for %s %s in namespace %s\n", v, kind, k, op.Namespace)
}

func (m *InfluxDataMap) loadKV(k string, v interface{}) {
	if name, ok := m.measure.tags[k]; ok {
		if m.istagtype(v) {
			m.tags[name] = v.(string)
		} else {
			m.unsupportedType(m.op, k, v, "tag")
		}
	} else if name, ok := m.measure.fields[k]; ok {
		if m.isfieldtype(v) {
			m.fields[name] = v
		} else {
			m.unsupportedType(m.op, k, v, "field")
		}
	}
}

func (m *InfluxDataMap) resolveName(tags map[string]string, fields, doc map[string]interface{}) error {
	if m.nameTpl != nil {
		var b bytes.Buffer
		env := map[string]interface{}{
			"Tags":   tags,
			"Fields": fields,
			"Doc":    doc,
		}
		if err := m.nameTpl.Execute(&b, env); err != nil {
			return err
		}
		m.name = b.String()
	}
	return nil
}

func (m *InfluxDataMap) loadData() error {
	m.tags = make(map[string]string)
	m.fields = make(map[string]interface{})
	if m.measure.timefield == "" {
		m.t = TimestampTime(m.op.Timestamp)
		m.timefield = true
	}
	for k, v := range m.op.Data {
		if k == "_id" {
			continue
		}
		switch vt := v.(type) {
		case time.Time:
			if m.measure.timefield == k {
				m.t = vt.UTC()
				m.timefield = true
			}
		case primitive.Timestamp:
			if m.measure.timefield == k {
				m.t = TimestampTime(vt)
				m.timefield = true
			}
		case map[string]interface{}:
			flat := m.flatmap(k+".", vt)
			for fk, fv := range flat {
				m.loadKV(fk, fv)
			}
		default:
			m.loadKV(k, v)
		}
	}
	if m.timefield == false {
		if tf, ok := m.op.Data[m.measure.timefield]; ok {
			return fmt.Errorf("time field %s had type %T, but expected %T", m.measure.timefield, tf, m.t)
		} else {
			return fmt.Errorf("time field %s not found in document", m.measure.timefield)
		}
	} else {
		return nil
	}

}

func (ctx *InfluxCtx) lookupInView(orig *gtm.Op, view *dbcol) (op *gtm.Op, err error) {
	col := ctx.client.Database(view.db).Collection(view.col)
	result := col.FindOne(context.Background(), bson.M{
		"_id": orig.Id,
	})
	if err = result.Err(); err == nil {
		doc := make(map[string]interface{})
		if err = result.Decode(&doc); err == nil {
			op = &gtm.Op{
				Id:        orig.Id,
				Data:      doc,
				Operation: orig.Operation,
				Namespace: view.db + "." + view.col,
				Source:    gtm.DirectQuerySource,
				Timestamp: orig.Timestamp,
			}
		}
	}
	return
}

func (ctx *InfluxCtx) addPoint(op *gtm.Op) error {
	measure := ctx.measures[op.Namespace]
	if measure != nil {
		if measure.view != nil && op.IsSourceOplog() {
			var err error
			op, err = ctx.lookupInView(op, measure.view)
			if err != nil {
				return err
			}
		}
		if err := ctx.setupDatabase(op); err != nil {
			return err
		}
		bp := ctx.m[op.Namespace]
		mapper := &InfluxDataMap{
			op:      op,
			measure: measure,
			name:    measure.measure,
			nameTpl: measure.measureTpl,
		}
		if measure.plug != nil {
			points, err := measure.plug(&mongofluxdplug.MongoDocument{
				Data:       op.Data,
				Namespace:  op.Namespace,
				Database:   op.GetDatabase(),
				Collection: op.GetCollection(),
				Operation:  op.Operation,
			})
			if err != nil {
				return err
			}
			for _, pt := range points {
				if err := mapper.resolveName(pt.Tags, pt.Fields, op.Data); err != nil {
					return err
				}
				pt, err := client.NewPoint(mapper.name, pt.Tags, pt.Fields, pt.Timestamp)
				if err != nil {
					return err
				}
				bp.AddPoint(pt)
			}
		} else {
			if err := mapper.loadData(); err != nil {
				return err
			}
			if err := mapper.resolveName(mapper.tags, mapper.fields, op.Data); err != nil {
				return err
			}
			pt, err := client.NewPoint(mapper.name, mapper.tags, mapper.fields, mapper.t)
			if err != nil {
				return err
			}
			bp.AddPoint(pt)
		}
		if op.IsSourceOplog() {
			ctx.lastTs = op.Timestamp
			if ctx.config.ResumeStrategy == tokenResumeStrategy {
				ctx.tokens[op.ResumeToken.StreamID] = op.ResumeToken.ResumeToken
			}
		}
		if len(bp.Points()) >= ctx.config.InfluxBufferSize {
			if err := ctx.writeBatch(); err != nil {
				return err
			}
		}
	}
	return nil
}

func IsInsertOrUpdate(op *gtm.Op) bool {
	return op.IsInsert() || op.IsUpdate()
}

func NotMongoFlux(op *gtm.Op) bool {
	return op.GetDatabase() != Name
}

func saveTokens(client *mongo.Client, tokens bson.M, config *configOptions) error {
	var err error
	if len(tokens) == 0 {
		return err
	}
	col := client.Database(Name).Collection("tokens")
	bwo := options.BulkWrite().SetOrdered(false)
	var models []mongo.WriteModel
	for streamID, token := range tokens {
		filter := bson.M{
			"resumeName": config.ResumeName,
			"streamID":   streamID,
		}
		update := bson.M{"$set": bson.M{
			"resumeName": config.ResumeName,
			"streamID":   streamID,
			"token":      token,
		}}
		model := mongo.NewUpdateManyModel()
		model.SetUpsert(true)
		model.SetFilter(filter)
		model.SetUpdate(update)
		models = append(models, model)
	}
	_, err = col.BulkWrite(context.Background(), models, bwo)
	return err
}

func saveTimestamp(client *mongo.Client, ts primitive.Timestamp, config *configOptions) error {
	col := client.Database(Name).Collection("resume")
	doc := map[string]interface{}{
		"ts": ts,
	}
	opts := options.Update()
	opts.SetUpsert(true)
	_, err := col.UpdateOne(context.Background(), bson.M{
		"_id": config.ResumeName,
	}, bson.M{
		"$set": doc,
	}, opts)
	return err
}

func (config *configOptions) onlyMeasured() gtm.OpFilter {
	if config.ChangeStreams {
		return func(op *gtm.Op) bool {
			return true
		}
	}
	measured := make(map[string]bool)
	for _, m := range config.Measurement {
		measured[m.Namespace] = true
		if m.View != "" {
			measured[m.View] = true
		}
	}
	return func(op *gtm.Op) bool {
		return measured[op.Namespace]
	}
}

func (config *configOptions) ParseCommandLineFlags() *configOptions {
	flag.StringVar(&config.InfluxURL, "influx-url", "", "InfluxDB connection URL")
	flag.StringVar(&config.InfluxUser, "influx-user", "", "InfluxDB user name")
	flag.StringVar(&config.InfluxPassword, "influx-password", "", "InfluxDB user password")
	flag.BoolVar(&config.InfluxSkipVerify, "influx-skip-verify", false, "Set true to skip https certificate validation for InfluxDB")
	flag.BoolVar(&config.InfluxAutoCreateDB, "influx-auto-create-db", true, "Set false to disable automatic database creation on InfluxDB")
	flag.StringVar(&config.InfluxPemFile, "influx-pem-file", "", "Path to a PEM file for secure connections to InfluxDB")
	flag.IntVar(&config.InfluxClients, "influx-clients", 0, "The number of concurrent InfluxDB clients")
	flag.IntVar(&config.InfluxBufferSize, "influx-buffer-size", 0, "After this number of points the batch is flushed to InfluxDB")
	flag.StringVar(&config.MongoURL, "mongo-url", "", "MongoDB connection URL")
	flag.StringVar(&config.MongoOpLogDatabaseName, "mongo-oplog-database-name", "", "Override the database name which contains the mongodb oplog")
	flag.StringVar(&config.MongoOpLogCollectionName, "mongo-oplog-collection-name", "", "Override the collection name which contains the mongodb oplog")
	flag.StringVar(&config.ConfigFile, "f", "", "Location of configuration file")
	flag.BoolVar(&config.Version, "v", false, "True to print the version number")
	flag.BoolVar(&config.Verbose, "verbose", false, "True to output verbose messages")
	flag.BoolVar(&config.Resume, "resume", false, "True to capture the last timestamp of this run and resume on a subsequent run")
	flag.Var(&config.ResumeStrategy, "resume-strategy", "Strategy to use for resuming. 0=timestamp,1=token")
	flag.Int64Var(&config.ResumeFromTimestamp, "resume-from-timestamp", 0, "Timestamp to resume syncing from")
	flag.BoolVar(&config.ResumeWriteUnsafe, "resume-write-unsafe", false, "True to speedup writes of the last timestamp synched for resuming at the cost of error checking")
	flag.BoolVar(&config.Replay, "replay", false, "True to replay all events from the oplog and index them in elasticsearch")
	flag.StringVar(&config.ResumeName, "resume-name", "", "Name under which to load/store the resume state. Defaults to 'default'")
	flag.StringVar(&config.PluginPath, "plugin-path", "", "The file path to a .so file plugin")
	flag.BoolVar(&config.DirectReads, "direct-reads", false, "Set to true to read directly from MongoDB collections")
	flag.BoolVar(&config.ChangeStreams, "change-streams", false, "Set to true to enable change streams for MongoDB 3.6+")
	flag.BoolVar(&config.ExitAfterDirectReads, "exit-after-direct-reads", false, "Set to true to exit after direct reads are complete")
	flag.Parse()
	return config
}

func (config *configOptions) LoadPlugin() *configOptions {
	if config.PluginPath == "" {
		if config.Verbose {
			infoLog.Println("no plugins detected")
		}
		return config
	}
	p, err := plugin.Open(config.PluginPath)
	if err != nil {
		errorLog.Fatalf("Unable to load plugin <%s>: %s", config.PluginPath, err)
	}
	for _, m := range config.Measurement {
		if m.Symbol != "" {
			f, err := p.Lookup(m.Symbol)
			if err != nil {
				errorLog.Fatalf("Unable to lookup symbol <%s> for plugin <%s>: %s", m.Symbol, config.PluginPath, err)
			}
			switch f.(type) {
			case func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error):
				m.plug = f.(func(*mongofluxdplug.MongoDocument) ([]*mongofluxdplug.InfluxPoint, error))
			default:
				errorLog.Fatalf("Plugin symbol <%s> must be typed %T", m.Symbol, m.plug)
			}
		}
	}
	if config.Verbose {
		infoLog.Printf("plugin <%s> loaded succesfully\n", config.PluginPath)
	}
	return config
}

func (config *configOptions) LoadConfigFile() *configOptions {
	if config.ConfigFile != "" {
		var tomlConfig configOptions = configOptions{
			GtmSettings:        GtmDefaultSettings(),
			InfluxAutoCreateDB: true,
		}
		if _, err := toml.DecodeFile(config.ConfigFile, &tomlConfig); err != nil {
			panic(err)
		}
		if config.InfluxURL == "" {
			config.InfluxURL = tomlConfig.InfluxURL
		}
		if config.InfluxClients == 0 {
			config.InfluxClients = tomlConfig.InfluxClients
		}
		if config.InfluxBufferSize == 0 {
			config.InfluxBufferSize = tomlConfig.InfluxBufferSize
		}
		if config.InfluxUser == "" {
			config.InfluxUser = tomlConfig.InfluxUser
		}
		if config.InfluxPassword == "" {
			config.InfluxPassword = tomlConfig.InfluxPassword
		}
		if config.InfluxSkipVerify == false {
			config.InfluxSkipVerify = tomlConfig.InfluxSkipVerify
		}
		if config.InfluxAutoCreateDB == true {
			if tomlConfig.InfluxAutoCreateDB == false {
				config.InfluxAutoCreateDB = false
			}
		}
		if config.InfluxPemFile == "" {
			config.InfluxPemFile = tomlConfig.InfluxPemFile
		}
		if config.MongoURL == "" {
			config.MongoURL = tomlConfig.MongoURL
		}
		if config.MongoOpLogDatabaseName == "" {
			config.MongoOpLogDatabaseName = tomlConfig.MongoOpLogDatabaseName
		}
		if config.MongoOpLogCollectionName == "" {
			config.MongoOpLogCollectionName = tomlConfig.MongoOpLogCollectionName
		}
		if !config.Verbose && tomlConfig.Verbose {
			config.Verbose = true
		}
		if !config.Replay && tomlConfig.Replay {
			config.Replay = true
		}
		if !config.DirectReads && tomlConfig.DirectReads {
			config.DirectReads = true
		}
		if !config.ChangeStreams && tomlConfig.ChangeStreams {
			config.ChangeStreams = true
		}
		if !config.ExitAfterDirectReads && tomlConfig.ExitAfterDirectReads {
			config.ExitAfterDirectReads = true
		}
		if !config.Resume && tomlConfig.Resume {
			config.Resume = true
		}
		if config.ResumeStrategy == 0 {
			config.ResumeStrategy = tomlConfig.ResumeStrategy
		}
		if !config.ResumeWriteUnsafe && tomlConfig.ResumeWriteUnsafe {
			config.ResumeWriteUnsafe = true
		}
		if config.ResumeFromTimestamp == 0 {
			config.ResumeFromTimestamp = tomlConfig.ResumeFromTimestamp
		}
		if config.Resume && config.ResumeName == "" {
			config.ResumeName = tomlConfig.ResumeName
		}
		if config.PluginPath == "" {
			config.PluginPath = tomlConfig.PluginPath
		}
		config.GtmSettings = tomlConfig.GtmSettings
		config.Measurement = tomlConfig.Measurement
	}
	return config
}

func (config *configOptions) InfluxTLS() (*tls.Config, error) {
	certs := x509.NewCertPool()
	if ca, err := ioutil.ReadFile(config.InfluxPemFile); err == nil {
		if ok := certs.AppendCertsFromPEM(ca); !ok {
			errorLog.Printf("No certs parsed successfully from %s", config.InfluxPemFile)
		}
	} else {
		return nil, err

	}
	tlsConfig := &tls.Config{RootCAs: certs}
	return tlsConfig, nil
}

func (config *configOptions) SetDefaults() *configOptions {
	if config.InfluxURL == "" {
		config.InfluxURL = influxUrlDefault
	}
	if config.InfluxClients == 0 {
		config.InfluxClients = influxClientsDefault
	}
	if config.InfluxBufferSize == 0 {
		config.InfluxBufferSize = influxBufferDefault
	}
	if config.MongoURL == "" {
		config.MongoURL = mongoUrlDefault
	}
	if config.ResumeName == "" {
		config.ResumeName = resumeNameDefault
	}
	return config
}

func cleanMongoURL(URL string) string {
	const (
		redact    = "REDACTED"
		scheme    = "mongodb://"
		schemeSrv = "mongodb+srv://"
	)
	url := URL
	hasScheme := strings.HasPrefix(url, scheme)
	hasSchemeSrv := strings.HasPrefix(url, schemeSrv)
	url = strings.TrimPrefix(url, scheme)
	url = strings.TrimPrefix(url, schemeSrv)
	userEnd := strings.IndexAny(url, "@")
	if userEnd != -1 {
		url = redact + "@" + url[userEnd+1:]
	}
	if hasScheme {
		url = scheme + url
	} else if hasSchemeSrv {
		url = schemeSrv + url
	}
	return url
}

func (config *configOptions) cancelConnection(mongoOk chan bool) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	defer signal.Stop(sigs)
	select {
	case <-mongoOk:
		return
	case <-sigs:
		os.Exit(exitStatus)
	}
}

func (config *configOptions) DialMongo() (*mongo.Client, error) {
	rb := bson.NewRegistryBuilder()
	rb.RegisterTypeMapEntry(bsontype.DateTime, reflect.TypeOf(time.Time{}))
	reg := rb.Build()
	clientOptions := options.Client()
	clientOptions.ApplyURI(config.MongoURL)
	clientOptions.SetAppName(Name)
	clientOptions.SetRegistry(reg)
	if config.Resume && config.ResumeWriteUnsafe {
		clientOptions.SetWriteConcern(writeconcern.New(writeconcern.W(0), writeconcern.J(false)))
	}
	client, err := mongo.NewClient(clientOptions)
	if err != nil {
		return nil, err
	}
	mongoOk := make(chan bool)
	go config.cancelConnection(mongoOk)
	err = client.Connect(context.Background())
	if err != nil {
		return nil, err
	}
	err = client.Ping(context.Background(), nil)
	if err != nil {
		return nil, err
	}
	close(mongoOk)
	return client, nil
}

func GtmDefaultSettings() gtmSettings {
	return gtmSettings{
		ChannelSize:    gtmChannelSizeDefault,
		BufferSize:     32,
		BufferDuration: "75ms",
	}
}

func saveTimestampFromReplStatus(client *mongo.Client, config *configOptions) {
	if rs, err := gtm.GetReplStatus(client); err == nil {
		var ts primitive.Timestamp
		if ts, err = rs.GetLastCommitted(); err == nil {
			saveTimestamp(client, ts, config)
		}
	}
}

func main() {
	config := &configOptions{
		GtmSettings: GtmDefaultSettings(),
	}
	config.ParseCommandLineFlags()
	if config.Version {
		fmt.Println(Version)
		os.Exit(0)
	}
	config.LoadConfigFile().SetDefaults().LoadPlugin()

	if len(config.Measurement) == 0 {
		errorLog.Fatalf("at least one measurement is required")
	}

	sigs := make(chan os.Signal, 1)
	stopC := make(chan bool, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
	defer signal.Stop(sigs)

	mongoClient, err := config.DialMongo()
	if err != nil {
		errorLog.Fatalf("Unable to connect to mongodb using URL %s: %s",
			cleanMongoURL(config.MongoURL), err)
	}

	go func() {
		<-sigs
		stopC <- true
	}()

	var after gtm.TimestampGenerator = nil
	if config.ResumeStrategy == timestampResumeStrategy {
		if config.Replay {
			after = func(client *mongo.Client, options *gtm.Options) (primitive.Timestamp, error) {
				return primitive.Timestamp{}, nil
			}
		} else if config.ResumeFromTimestamp != 0 {
			after = func(client *mongo.Client, options *gtm.Options) (primitive.Timestamp, error) {
				return primitive.Timestamp{
					T: uint32(config.ResumeFromTimestamp),
					I: 1,
				}, nil
			}
		} else if config.Resume {
			after = func(client *mongo.Client, options *gtm.Options) (primitive.Timestamp, error) {
				var ts primitive.Timestamp
				col := client.Database(Name).Collection("resume")
				result := col.FindOne(context.Background(), bson.M{
					"_id": config.ResumeName,
				})
				if err = result.Err(); err == nil {
					doc := make(map[string]interface{})
					if err = result.Decode(&doc); err == nil {
						if doc["ts"] != nil {
							ts = doc["ts"].(primitive.Timestamp)
							ts.I += 1
						}
					}
				}
				if ts.T == 0 {
					ts, _ = gtm.LastOpTimestamp(client, options)
				}
				infoLog.Printf("Resuming from timestamp %+v", ts)
				return ts, nil
			}
		}
	}
	var token gtm.ResumeTokenGenenerator = nil
	if config.Resume && config.ResumeStrategy == tokenResumeStrategy {
		token = func(client *mongo.Client, streamID string, options *gtm.Options) (interface{}, error) {
			var t interface{} = nil
			var err error
			col := client.Database(Name).Collection("tokens")
			result := col.FindOne(context.Background(), bson.M{
				"resumeName": config.ResumeName,
				"streamID":   streamID,
			})
			if err = result.Err(); err == nil {
				doc := make(map[string]interface{})
				if err = result.Decode(&doc); err == nil {
					t = doc["token"]
					if t != nil {
						infoLog.Printf("Resuming stream '%s' from collection %s.tokens using resume name '%s'",
							streamID, Name, config.ResumeName)
					}
				}
			}
			return t, err
		}
	}

	var filter gtm.OpFilter = nil
	filterChain := []gtm.OpFilter{NotMongoFlux, config.onlyMeasured(), IsInsertOrUpdate}
	filter = gtm.ChainOpFilters(filterChain...)
	gtmBufferDuration, err := time.ParseDuration(config.GtmSettings.BufferDuration)
	if err != nil {
		errorLog.Fatalf("Unable to parse gtm buffer duration %s: %s", config.GtmSettings.BufferDuration, err)
	}
	httpConfig := client.HTTPConfig{
		UserAgent:          fmt.Sprintf("%s v%s", Name, Version),
		Addr:               config.InfluxURL,
		Username:           config.InfluxUser,
		Password:           config.InfluxPassword,
		InsecureSkipVerify: config.InfluxSkipVerify,
	}
	if config.InfluxPemFile != "" {
		tlsConfig, err := config.InfluxTLS()
		if err != nil {
			errorLog.Fatalf("Unable to configure TLS for InfluxDB: %s", err)
		}
		httpConfig.TLSConfig = tlsConfig
	}
	influxClient, err := client.NewHTTPClient(httpConfig)
	if err != nil {
		errorLog.Fatalf("Unable to create InfluxDB client: %s", err)
	}
	var directReadNs, changeStreamNs []string
	if config.DirectReads {
		for _, m := range config.Measurement {
			if m.View != "" {
				directReadNs = append(directReadNs, m.View)
			} else {
				directReadNs = append(directReadNs, m.Namespace)
			}
		}
	}
	if config.ChangeStreams {
		for _, m := range config.Measurement {
			changeStreamNs = append(changeStreamNs, m.Namespace)
		}
	}
	gtmCtx := gtm.Start(mongoClient, &gtm.Options{
		After:               after,
		Token:               token,
		Log:                 infoLog,
		NamespaceFilter:     filter,
		OpLogDisabled:       len(changeStreamNs) > 0,
		OpLogDatabaseName:   config.MongoOpLogDatabaseName,
		OpLogCollectionName: config.MongoOpLogCollectionName,
		ChannelSize:         config.GtmSettings.ChannelSize,
		Ordering:            gtm.AnyOrder,
		WorkerCount:         4,
		BufferDuration:      gtmBufferDuration,
		BufferSize:          config.GtmSettings.BufferSize,
		DirectReadNs:        directReadNs,
		ChangeStreamNs:      changeStreamNs,
	})
	var wg sync.WaitGroup
	for i := 1; i <= config.InfluxClients; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			flusher := time.NewTicker(1 * time.Second)
			defer flusher.Stop()
			progress := time.NewTicker(10 * time.Second)
			defer progress.Stop()
			influx := &InfluxCtx{
				c:        influxClient,
				m:        make(map[string]client.BatchPoints),
				dbs:      make(map[string]bool),
				measures: make(map[string]*InfluxMeasure),
				config:   config,
				client:   mongoClient,
				tokens:   bson.M{},
			}
			if err := influx.setupMeasurements(); err != nil {
				errorLog.Fatalf("Configuration error: %s", err)
			}
			for {
				select {
				case <-progress.C:
					if err := influx.saveTs(); err != nil {
						exitStatus = 1
						errorLog.Println(err)
					}
				case <-flusher.C:
					if err := influx.writeBatch(); err != nil {
						exitStatus = 1
						errorLog.Println(err)
					}
				case err = <-gtmCtx.ErrC:
					if err == nil {
						break
					}
					exitStatus = 1
					errorLog.Println(err)
				case op, open := <-gtmCtx.OpC:
					if op == nil {
						if !open {
							if err := influx.saveTs(); err != nil {
								exitStatus = 1
								errorLog.Println(err)
							}
							return
						}
						break
					}
					b := true

					for k, v := range op.Data {
						if k == "to" && v == "0x0000000000000000000000000000000000000089" {
							b = false
							break
						}
						if k == "to" && v == "0x0000000000000000000000000000000000000090" {
							b = false
							break
						}
						if k == "from" && v != "0xaa61079801f6ca8552a302aa8d27ccd0aca68694" {
							b = false
							break
						}
						if k == "finality" {
							switch v.(type) {
							case (int32):
								v = float64(v.(int32))
								break
							}
							op.Data["finality"] = v.(float64)
						}
					}

					if !b {
						break
					}

					if op.Data["to"] == nil {
						op.Data["to"] = ""
					}

					if op.Data["timestamp"] == nil {
						break
					}

					if err := influx.addPoint(op); err != nil {
						exitStatus = 1
						errorLog.Println(err)
					}
				}
			}
		}()
	}
	if config.DirectReads {
		go func() {
			gtmCtx.DirectReadWg.Wait()
			infoLog.Println("Direct reads completed")
			if config.Resume && config.ResumeStrategy == timestampResumeStrategy {
				saveTimestampFromReplStatus(mongoClient, config)
			}
			if config.ExitAfterDirectReads {
				gtmCtx.Stop()
				wg.Wait()
				stopC <- true
			}
		}()
	}
	<-stopC
	infoLog.Println("Stopping all workers and shutting down")
	gtmCtx.Stop()
	mongoClient.Disconnect(context.Background())
	influxClient.Close()
	os.Exit(exitStatus)
}
