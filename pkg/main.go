package main

import (
	"flag"
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/spf13/viper"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// GeneralConfig has miscelaneous configuration options
type GeneralConfig struct {
	InstanceID string `toml:"instanceID"`
	LogDir     string `toml:"logdir"`
	LogLevel   string `toml:"loglevel"`
}

var (
	version    string
	commit     string
	branch     string
	buildstamp string
)

var (
	log        = logrus.New()
	quit       = make(chan struct{})
	startTime  = time.Now()
	showConfig bool
	getversion bool
	httpPort   = 8080
	appdir     = os.Getenv("PWD")
	logDir     = filepath.Join(appdir, "log")
	confDir    = filepath.Join(appdir, "conf")
	configFile = filepath.Join(confDir, "config.toml")

	cfg = struct {
		General      GeneralConfig
		Database     DatabaseCfg
		Selfmon      SelfMonConfig
		Metrics      map[string]*SnmpMetricCfg
		Measurements map[string]*InfluxMeasurementCfg
		MFilters     map[string]*MeasFilterCfg
		GetGroups    map[string]*MGroupsCfg
		SnmpDevice   map[string]*SnmpDeviceCfg
		Influxdb     map[string]*InfluxCfg
		HTTP         HTTPConfig
	}{}
	//mutex for devices map
	mutex sync.Mutex
	//runtime devices
	devices map[string]*SnmpDevice
	//runtime output db's
	influxdb map[string]*InfluxDB
	// for synchronize  deivce specific goroutines
	GatherWg  sync.WaitGroup
	SelfmonWg sync.WaitGroup
	//SenderWg  sync.WaitGroup
)

func flags() *flag.FlagSet {
	var f flag.FlagSet
	f.BoolVar(&getversion, "version", getversion, "display de version")
	f.BoolVar(&showConfig, "showconf", showConfig, "show all devices config and exit")
	f.StringVar(&configFile, "config", configFile, "config file")
	f.IntVar(&httpPort, "http", httpPort, "http port")
	f.StringVar(&logDir, "logs", logDir, "log directory")
	f.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		f.VisitAll(func(flag *flag.Flag) {
			format := "%10s: %s\n"
			fmt.Fprintf(os.Stderr, format, "-"+flag.Name, flag.Usage)
		})
		fmt.Fprintf(os.Stderr, "\nAll settings can be set in config file: %s\n", configFile)
		os.Exit(1)

	}
	return &f
}

/*
initMetricsCfg this function does 2 things
1.- Initialice id from key of maps for all SnmpMetricCfg and InfluxMeasurementCfg objects
2.- Initialice references between InfluxMeasurementCfg and SnmpMetricGfg objects
*/

func initMetricsCfg() error {
	//TODO:
	// - check duplicates OID's => warning messages
	//Initialize references to SnmpMetricGfg into InfluxMeasurementCfg
	log.Debug("--------------------Initializing Config metrics-------------------")
	log.Debug("Initializing SNMPMetricconfig...")
	for mKey, mVal := range cfg.Metrics {
		err := mVal.Init()
		if err != nil {
			log.Warnln("Error in Metric config:", err)
			//if some error int the format the metric is deleted from the config
			delete(cfg.Metrics, mKey)
		}
	}
	log.Debug("Initializing MEASSUREMENTSconfig...")
	for mKey, mVal := range cfg.Measurements {
		err := mVal.Init(&cfg.Metrics)
		if err != nil {
			log.Warnln("Error in Measurement config:", err)
			//if some error int the format the metric is deleted from the config
			delete(cfg.Metrics, mKey)
		}

		log.Debugf("FIELDMETRICS: %+v", mVal.fieldMetric)
	}
	log.Debug("-----------------------END Config metrics----------------------")
	return nil
}

//PrepareInfluxDBs review all configured db's in the SQL database
// and check if exist at least a "default", if not creates a dummy db which does nothing
func PrepareInfluxDBs() map[string]*InfluxDB {
	idb := make(map[string]*InfluxDB)

	var defFound bool
	for k, c := range cfg.Influxdb {
		//Inticialize each SNMP device
		if k == "default" {
			defFound = true
		}
		dev := InfluxDB{
			cfg:       c,
			dummy:     false,
			initrefs:  0,
			startrefs: 0,
			Sent:      0,
			Errors:    0,
		}
		idb[k] = &dev
	}
	if defFound == false {
		//no devices configured  as default device we need to set some device as itcan send data transparant to snmpdevices goroutines
		log.Warn("No Output default found influxdb devices found !!")
		idb["default"] = influxdbDummy
	}
	return idb
}

func init() {
	//Log format
	customFormatter := new(logrus.TextFormatter)
	customFormatter.TimestampFormat = "2006-01-02 15:04:05"
	log.Formatter = customFormatter
	customFormatter.FullTimestamp = true
	//----

	// parse first time to see if config file is being specified
	f := flags()
	f.Parse(os.Args[1:])

	if getversion {
		t, _ := strconv.ParseInt(buildstamp, 10, 64)
		fmt.Printf("snmpcollector v%s (git: %s ) built at [%s]\n", version, commit, time.Unix(t, 0).Format("2006-01-02 15:04:05"))
		os.Exit(0)
	}

	// now load up config settings
	if _, err := os.Stat(configFile); err == nil {
		viper.SetConfigFile(configFile)
		confDir = filepath.Dir(configFile)
	} else {
		viper.SetConfigName("config")
		viper.AddConfigPath("/opt/snmpcollector/conf/")
		viper.AddConfigPath("./conf/")
		viper.AddConfigPath(".")
	}
	err := viper.ReadInConfig()
	if err != nil {
		log.Errorf("Fatal error config file: %s \n", err)
		os.Exit(1)
	}
	err = viper.Unmarshal(&cfg)
	if err != nil {
		log.Errorf("Fatal error config file: %s \n", err)
		os.Exit(1)
	}

	if len(cfg.General.LogDir) > 0 {
		logDir = cfg.General.LogDir
		os.Mkdir(logDir, 0755)
	}
	if len(cfg.General.LogLevel) > 0 {
		l, _ := logrus.ParseLevel(cfg.General.LogLevel)
		log.Level = l
	}
	//
	log.Infof("Set Default directories : \n   - Exec: %s\n   - Config: %s\n   -Logs: %s\n", appdir, confDir, logDir)
}

//GetDevice is a safe method to get a Device Object
func GetDevice(id string) (*SnmpDevice, error) {
	var dev *SnmpDevice
	var ok bool
	mutex.Lock()
	if dev, ok = devices[id]; !ok {
		return nil, fmt.Errorf("there is not any device with id %s running", id)
	}
	mutex.Unlock()
	return dev, nil
}

func GetDevStats() map[string]*devStat {
	devstats := make(map[string]*devStat)
	mutex.Lock()
	for k, v := range devices {
		devstats[k] = v.GetBasicStats()
	}
	mutex.Unlock()
	return devstats
}

/*
func StopInfluxOut(idb map[string]*InfluxDB) {
	for k, v := range idb {
		log.Infof("Stopping Influxdb out %s", k)
		v.StopSender()
	}
}

func ReleaseInfluxOut(idb map[string]*InfluxDB) {
	for k, v := range idb {
		log.Infof("Release Influxdb resources %s", k)
		v.End()
	}
}
*/
// ProcessStop stop all device goroutines
func DeviceProcessStop() {
	mutex.Lock()
	for _, d := range devices {
		d.StopGather()
	}
	mutex.Unlock()
}

// ProcessStart start all devices goroutines
func DeviceProcessStart() {
	mutex.Lock()
	for _, d := range devices {

		d.StartGather(&GatherWg)
	}
	mutex.Unlock()
}

func ReleaseDevices() {
	mutex.Lock()
	for _, c := range devices {
		c.End()
	}
	mutex.Unlock()
}

// LoadConf call to initialize alln configurations
func LoadConf() {
	//Load all database info to Cfg struct
	cfg.Database.LoadConfig()
	//Prepare the InfluxDataBases Configuration
	influxdb = PrepareInfluxDBs()

	//Initialize Device Metrics CFG

	initMetricsCfg()

	//Initialize Device Runtime map

	// only run when one needs to see the interface names of the device
	if showConfig {
		mutex.Lock()
		for _, c := range devices {
			fmt.Println("\nSNMP host:", c.cfg.ID)
			fmt.Println("=========================================")
			c.printConfig()
		}
		mutex.Unlock()
		os.Exit(0)
	}
	//beginning  the gather process
}

func AgentStop() time.Duration {
	start := time.Now()
	log.Info("Agent begin device Gather processes stop...")
	DeviceProcessStop()
	log.Info("Agent waiting for all Gather gorotines stop...")
	GatherWg.Wait()
	log.Info("Agent releasing Device Resources")
	mutex.Lock()
	for k, dev := range devices {
		log.Infof("Stopping out sender for device %s", k)
		//Inticialize each SNMP device and put pointer to the global map devices
		outdb, _ := dev.GetOutSenderFromMap(influxdb)
		outdb.StopSender() //Sinc operaton blocks until really stopped
		outdb.End()
	}
	mutex.Unlock()

	ReleaseDevices()

	return time.Since(start)
}

func AgentStart() time.Duration {
	start := time.Now()
	devices = make(map[string]*SnmpDevice)

	for k, c := range cfg.SnmpDevice {
		//Inticialize each SNMP device and put pointer to the global map devices
		dev := NewSnmpDevice(c)
		dev.SetSelfMonitoring(&cfg.Selfmon)
		if !showConfig {
			outdb, _ := dev.GetOutSenderFromMap(influxdb)
			outdb.Init()
			outdb.StartSender( /*&SenderWg*/ )
		}

		mutex.Lock()
		devices[k] = dev
		mutex.Unlock()
	}
	log.Info("Agent Starting all device processes ...")
	DeviceProcessStart()
	return time.Since(start)
}

func SelfMonStop() time.Duration {
	start := time.Now()
	log.Info("Selmon: begin selfmon Gather processes stop...")
	//stop the selfmon process
	cfg.Selfmon.StopGather()
	log.Info("Selfmon: waiting for SelfMon Gather gorotines stop...")
	//wait until Done
	SelfmonWg.Wait()
	log.Info("Selfmon: releasing Seflmonitoring Resources")

	outdb := cfg.Selfmon.getOutput()
	cfg.Selfmon.End()

	outdb.StopSender() //Blocking operation
	outdb.End()

	return time.Since(start)
}

func SelfMonStart() time.Duration {
	start := time.Now()
	idb := influxdb
	log.Debugf("INFLUXDB ARRAY: %+v", idb)
	if cfg.Selfmon.Enabled && !showConfig {
		if outdb, ok := idb["default"]; ok {
			//only executed if a "default" influxdb exist
			log.Debugf("INFLUXDB  OUT: %+v", outdb)
			outdb.Init()
			outdb.StartSender()

			cfg.Selfmon.Init()
			cfg.Selfmon.setOutput(outdb)

			log.Printf("SELFMON enabled %+v", cfg.Selfmon)
			//Begin the statistic reporting
			cfg.Selfmon.StartGather(&SelfmonWg)
		} else {
			cfg.Selfmon.Enabled = false
			log.Errorf("SELFMON disabled becaouse of no default db found !!! SELFMON[ %+v ]  INFLUXLIST[ %+v]\n", cfg.Selfmon, idb)
		}
	} else {
		log.Printf("SELFMON disabled %+v\n", cfg.Selfmon)
	}

	return time.Since(start)
}

/*
func SenderStop() time.Duration {
	start := time.Now()
	log.Info("Sender: begin sender processes stop...")
	//stop all Output Emmiter
	StopInfluxOut(influxdb)
	log.Info("Sender: waiting for all Sender gorotines stop..")
	SenderWg.Wait()
	log.Info("Sender: releasing Sender Resources")
	ReleaseInfluxOut(influxdb)
	return time.Since(start)
}*/

// ReloadConf call to reinitialize alln configurations
func ReloadConf() time.Duration {
	start := time.Now()
	AgentStop() //

	SelfMonStop()

	//SenderStop()

	log.Info("RELOADCONF: ĺoading configuration Again...")
	LoadConf()

	SelfMonStart()

	AgentStart() //include initialize and Start Senders

	return time.Since(start)
}

func main() {

	defer func() {
		//errorLog.Close()
	}()
	//Init BD config

	InitDB(&cfg.Database)

	LoadConf()

	SelfMonStart()

	AgentStart() //include Sender Start

	var port int
	if cfg.HTTP.Port > 0 {
		port = cfg.HTTP.Port
	} else {
		port = httpPort
	}

	if port > 0 {
		webServer(port)
	} else {
		<-quit
	}
}
