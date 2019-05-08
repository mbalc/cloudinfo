// Copyright © 2018 Banzai Cloud
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package main Product Info.
//
// The product info application uses the cloud provider APIs to asynchronously fetch and parse instance type attributes
// and prices, while storing the results in an in memory cache and making it available as structured data through a REST API.
//
//     Schemes: http, https
//     BasePath: /api/v1
//     Version: 0.0.1
//     License: Apache 2.0 http://www.apache.org/licenses/LICENSE-2.0.html
//     Contact: Banzai Cloud<info@banzaicloud.com>
//
// swagger:meta
package main

import (
	"fmt"
	"os"

	"github.com/gin-gonic/gin"
	"github.com/goph/emperror"
	"github.com/goph/logur"
	"github.com/pkg/errors"
	"github.com/spf13/pflag"
	"github.com/spf13/viper"

	"github.com/banzaicloud/cloudinfo/internal/app/cloudinfo/api"
	"github.com/banzaicloud/cloudinfo/internal/app/cloudinfo/cistore"
	"github.com/banzaicloud/cloudinfo/internal/app/cloudinfo/loader"
	"github.com/banzaicloud/cloudinfo/internal/app/cloudinfo/management"
	"github.com/banzaicloud/cloudinfo/internal/app/cloudinfo/messaging"
	"github.com/banzaicloud/cloudinfo/internal/app/cloudinfo/tracing"
	cloudinfo2 "github.com/banzaicloud/cloudinfo/internal/cloudinfo"
	"github.com/banzaicloud/cloudinfo/internal/cloudinfo/cloudinfodriver"
	"github.com/banzaicloud/cloudinfo/internal/platform/buildinfo"
	"github.com/banzaicloud/cloudinfo/internal/platform/errorhandler"
	"github.com/banzaicloud/cloudinfo/internal/platform/log"
	"github.com/banzaicloud/cloudinfo/pkg/cloudinfo"
	"github.com/banzaicloud/cloudinfo/pkg/cloudinfo/alibaba"
	"github.com/banzaicloud/cloudinfo/pkg/cloudinfo/amazon"
	"github.com/banzaicloud/cloudinfo/pkg/cloudinfo/azure"
	"github.com/banzaicloud/cloudinfo/pkg/cloudinfo/google"
	"github.com/banzaicloud/cloudinfo/pkg/cloudinfo/metrics"
	"github.com/banzaicloud/cloudinfo/pkg/cloudinfo/oracle"
)

// Provisioned by ldflags
// nolint: gochecknoglobals
var (
	version    string
	commitHash string
	buildDate  string
)

func main() {
	v, p := viper.New(), pflag.NewFlagSet(friendlyAppName, pflag.ExitOnError)
	configure(v, p)

	p.String("config", "", "Configuration file")
	p.Bool("version", false, "Show version information")
	p.Bool("dump-config", false, "Dump configuration to the console (and exit)")

	_ = p.Parse(os.Args[1:])

	if v, _ := p.GetBool("version"); v {
		fmt.Printf("%s version %s (%s) built on %s\n", friendlyAppName, version, commitHash, buildDate)

		os.Exit(0)
	}

	if c, _ := p.GetString("config"); c != "" {
		v.SetConfigFile(c)
	}

	err := v.ReadInConfig()
	_, configFileNotFound := err.(viper.ConfigFileNotFoundError)
	if !configFileNotFound {
		emperror.Panic(errors.Wrap(err, "failed to read configuration"))
	}

	var config configuration
	err = v.Unmarshal(&config)
	emperror.Panic(errors.Wrap(err, "failed to unmarshal configuration"))

	// Create logger (first thing after configuration loading)
	logger := log.NewLogger(config.Log)

	// Provide some basic context to all log lines
	logger = log.WithFields(logger, map[string]interface{}{"environment": config.Environment, "application": appName})

	log.SetStandardLogger(logger)

	if configFileNotFound {
		logger.Warn("configuration file not found")
	}

	err = config.Validate()
	if err != nil {
		logger.Error(err.Error())

		os.Exit(3)
	}

	if d, _ := pflag.CommandLine.GetBool("dump-config"); d {
		fmt.Printf("%+v\n", config)

		os.Exit(0)
	}

	// Configure error handler
	errorHandler := errorhandler.New(logger)
	defer emperror.HandleRecover(errorHandler)

	buildInfo := buildinfo.New(version, commitHash, buildDate)

	logger.Info("starting application", buildInfo.Fields())

	// default tracer
	tracer := tracing.NewNoOpTracer()

	// Configure Jaeger
	if config.Jaeger.Enabled {
		logger.Info("jaeger exporter enabled")

		tracing.SetupTracing(config.Jaeger.Config, emperror.NewNoopHandler())
		tracer = tracing.NewTracer()
	}

	// use the configured store implementation
	cloudInfoStore := cistore.NewCloudInfoStore(config.Store, logger)
	defer cloudInfoStore.Close()

	infoers, providers, err := loadInfoers(config, logger)
	emperror.Panic(err)

	reporter := metrics.NewDefaultMetricsReporter()

	eventBus := messaging.NewDefaultEventBus()

	serviceManager := loader.NewDefaultServiceManager(config.ServiceLoader, cloudInfoStore, logger, eventBus)
	serviceManager.ConfigureServices(providers)

	serviceManager.LoadServiceInformation(providers)

	prodInfo, err := cloudinfo.NewCachingCloudInfo(infoers, cloudInfoStore, logger)
	emperror.Panic(err)

	scrapingDriver := cloudinfo.NewScrapingDriver(config.Scrape.Interval, infoers, cloudInfoStore, logger, reporter, tracer, eventBus)

	err = scrapingDriver.StartScraping()
	emperror.Panic(err)

	// start the management service
	if config.Management.Enabled {
		go management.StartManagementEngine(config.Management, cloudInfoStore, *scrapingDriver, logger)
	}

	err = api.ConfigureValidator(providers, prodInfo, logger)
	emperror.Panic(err)

	providerService := cloudinfo2.NewProviderService(prodInfo)
	instanceTypeService := cloudinfo2.NewInstanceTypeService(prodInfo)
	endpoints := cloudinfodriver.MakeEndpoints(providerService, instanceTypeService)
	graphqlHandler := cloudinfodriver.MakeGraphQLHandler(endpoints, errorHandler)

	routeHandler := api.NewRouteHandler(prodInfo, buildInfo, graphqlHandler, logger)

	// new default gin engine (recovery, logger middleware)
	router := gin.Default()

	// add prometheus metric endpoint
	if config.Metrics.Enabled {
		logger.Info("metrics enabled")

		routeHandler.EnableMetrics(router, config.Metrics.Address)
	}

	routeHandler.ConfigureRoutes(router)

	err = router.Run(config.App.Address)
	emperror.Panic(errors.Wrap(err, "failed to run router"))
}

func loadInfoers(config configuration, logger logur.Logger) (map[string]cloudinfo.CloudInfoer, []string, error) {
	infoers := map[string]cloudinfo.CloudInfoer{}

	var providers []string

	if config.Provider.Amazon.Enabled {
		providers = append(providers, Amazon)
		logger := logur.WithFields(logger, map[string]interface{}{"provider": Amazon})

		infoer, err := amazon.NewAmazonInfoer(config.Provider.Amazon.Config, logger)
		if err != nil {
			return nil, nil, emperror.With(err, "provider", Amazon)
		}

		infoers[Amazon] = infoer

		logger.Info("configured cloud info provider")
	}

	if config.Provider.Google.Enabled {
		providers = append(providers, Google)
		logger := logur.WithFields(logger, map[string]interface{}{"provider": Google})

		infoer, err := google.NewGoogleInfoer(config.Provider.Google.Config, logger)
		if err != nil {
			return nil, nil, emperror.With(err, "provider", Google)
		}

		infoers[Google] = infoer

		logger.Info("configured cloud info provider")
	}

	if config.Provider.Alibaba.Enabled {
		providers = append(providers, Alibaba)
		logger := logur.WithFields(logger, map[string]interface{}{"provider": Alibaba})

		infoer, err := alibaba.NewAlibabaInfoer(config.Provider.Alibaba.Config, logger)
		if err != nil {
			return nil, nil, emperror.With(err, "provider", Alibaba)
		}

		infoers[Alibaba] = infoer

		logger.Info("configured cloud info provider")
	}

	if config.Provider.Oracle.Enabled {
		providers = append(providers, Oracle)
		logger := logur.WithFields(logger, map[string]interface{}{"provider": Oracle})

		infoer, err := oracle.NewOracleInfoer(config.Provider.Oracle.Config, logger)
		if err != nil {
			return nil, nil, emperror.With(err, "provider", Oracle)
		}

		infoers[Oracle] = infoer

		logger.Info("configured cloud info provider")
	}

	if config.Provider.Azure.Enabled {
		providers = append(providers, Azure)
		logger := logur.WithFields(logger, map[string]interface{}{"provider": Azure})

		infoer, err := azure.NewAzureInfoer(config.Provider.Azure.Config, logger)
		if err != nil {
			return nil, nil, emperror.With(err, "provider", Azure)
		}

		infoers[Azure] = infoer

		logger.Info("configured cloud info provider")
	}

	return infoers, providers, nil
}
