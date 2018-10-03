package main

import (
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"lib/common"
	"lib/nonmutualtls"
	"lib/poller"

	"policy-server/adapter"
	"policy-server/api"
	"policy-server/api/api_v0"
	"policy-server/cc_client"
	"policy-server/cleaner"
	"policy-server/config"
	"policy-server/handlers"
	psmiddleware "policy-server/middleware"
	"policy-server/store"
	"policy-server/uaa_client"

	"code.cloudfoundry.org/cf-networking-helpers/db"
	"code.cloudfoundry.org/cf-networking-helpers/httperror"
	"code.cloudfoundry.org/cf-networking-helpers/json_client"
	"code.cloudfoundry.org/cf-networking-helpers/marshal"
	"code.cloudfoundry.org/cf-networking-helpers/metrics"
	"code.cloudfoundry.org/cf-networking-helpers/middleware"
	middlewareAdapter "code.cloudfoundry.org/cf-networking-helpers/middleware/adapter"
	"code.cloudfoundry.org/debugserver"
	"code.cloudfoundry.org/lager"
	"code.cloudfoundry.org/lager/lagerflags"
	"github.com/cloudfoundry/dropsonde"
	"github.com/tedsuo/ifrit"
	"github.com/tedsuo/ifrit/grouper"
	"github.com/tedsuo/ifrit/sigmon"
	"github.com/tedsuo/rata"
)

const (
	jobPrefix       = "policy-server"
	dropsondeOrigin = "policy-server"
)

var (
	logPrefix = "cfnetworking"
)

func main() {
	configFilePath := flag.String("config-file", "", "path to config file")
	flag.Parse()

	conf, err := config.New(*configFilePath)
	if err != nil {
		log.Fatalf("%s.%s: could not read config file: %s", logPrefix, jobPrefix, err)
	}

	if conf.LogPrefix != "" {
		logPrefix = conf.LogPrefix
	}

	logger, reconfigurableSink := lagerflags.NewFromConfig(fmt.Sprintf("%s.%s", logPrefix, jobPrefix), common.GetLagerConfig())

	var tlsConfig *tls.Config
	if conf.SkipSSLValidation {
		tlsConfig = &tls.Config{
			InsecureSkipVerify: conf.SkipSSLValidation,
		}
	} else {
		tlsConfig, err = nonmutualtls.NewClientTLSConfig(conf.UAACA, conf.CCCA)
		if err != nil {
			log.Fatalf("%s.%s error creating tls config: %s", logPrefix, jobPrefix, err) // not tested
		}
	}
	httpClient := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
		},
	}

	uaaClient := &uaa_client.Client{
		BaseURL:    fmt.Sprintf("%s:%d", conf.UAAURL, conf.UAAPort),
		Name:       conf.UAAClient,
		Secret:     conf.UAAClientSecret,
		HTTPClient: httpClient,
		Logger:     logger,
	}

	whoamiHandler := &handlers.WhoAmIHandler{
		Marshaler: marshal.MarshalFunc(json.Marshal),
	}

	uptimeHandler := &handlers.UptimeHandler{
		StartTime: time.Now(),
	}

	storeGroup := &store.GroupTable{}
	destination := &store.DestinationTable{}
	policy := &store.PolicyTable{}

	logger.Info("getting db connection", lager.Data{})
	connectionPool, err := db.NewConnectionPool(
		conf.Database,
		conf.MaxOpenConnections,
		conf.MaxIdleConnections,
		time.Duration(conf.MaxConnectionsLifetimeSeconds)*time.Second,
		logPrefix,
		jobPrefix,
		logger,
	)
	if err != nil {
		log.Fatalf(err.Error())
	}

	logger.Info("db connection retrieved", lager.Data{})

	terminalsTable := &store.TerminalsTable{
		Guids: &store.GuidGenerator{},
	}
	egressPolicyStore := &store.EgressPolicyStore{
		EgressPolicyRepo: &store.EgressPolicyTable{
			Conn:  connectionPool,
			Guids: &store.GuidGenerator{},
		},
		TerminalsRepo: terminalsTable,
		Conn:          connectionPool,
	}

	c2cPolicyStore := store.New(
		connectionPool,
		storeGroup,
		destination,
		policy,
		conf.TagLength,
	)

	if err != nil {
		log.Fatalf("%s.%s: failed to construct datastore: %s", logPrefix, jobPrefix, err) // not tested
	}

	tagDataStore := store.NewTagStore(connectionPool, &store.GroupTable{}, conf.TagLength)

	metricsSender := &metrics.MetricsSender{
		Logger: logger.Session("time-metric-emitter"),
	}

	wrappedStore := &store.MetricsWrapper{
		Store:         c2cPolicyStore,
		TagStore:      tagDataStore,
		MetricsSender: metricsSender,
	}

	errorResponse := &httperror.ErrorResponse{
		MetricsSender: metricsSender,
	}

	ccClient := &cc_client.Client{
		JSONClient: json_client.New(logger.Session("cc-json-client"), httpClient, conf.CCURL),
		Logger:     logger,
	}

	policyGuard := handlers.NewPolicyGuard(uaaClient, ccClient)
	quotaGuard := handlers.NewQuotaGuard(wrappedStore, conf.MaxPolicies)
	policyFilter := handlers.NewPolicyFilter(uaaClient, ccClient, 100)

	policyMapperV0 := api_v0.NewMapper(marshal.UnmarshalFunc(json.Unmarshal), marshal.MarshalFunc(json.Marshal), &api_v0.Validator{})
	policyMapperV1 := api.NewMapper(marshal.UnmarshalFunc(json.Unmarshal), marshal.MarshalFunc(json.Marshal), &api.PolicyValidator{})

	createPolicyHandlerV1 := handlers.NewPoliciesCreate(wrappedStore, policyMapperV1,
		policyGuard, quotaGuard, errorResponse)
	createPolicyHandlerV0 := handlers.NewPoliciesCreate(wrappedStore, policyMapperV0,
		policyGuard, quotaGuard, errorResponse)

	deletePolicyHandlerV1 := handlers.NewPoliciesDelete(wrappedStore, policyMapperV1,
		policyGuard, errorResponse)
	deletePolicyHandlerV0 := handlers.NewPoliciesDelete(wrappedStore, policyMapperV0,
		policyGuard, errorResponse)

	policiesIndexHandlerV1 := handlers.NewPoliciesIndex(wrappedStore, policyMapperV1, policyFilter, policyGuard, errorResponse)
	policiesIndexHandlerV0 := handlers.NewPoliciesIndex(wrappedStore, policyMapperV0, policyFilter, policyGuard, errorResponse)

	egressDestinationMapper := &api.EgressDestinationMapper{
		Marshaler:        marshal.MarshalFunc(json.Marshal),
		PayloadValidator: &api.EgressDestinationsValidator{},
	}

	egressDestinationStore := &store.EgressDestinationStore{
		Conn: connectionPool,
		EgressDestinationRepo:   &store.EgressDestinationTable{},
		TerminalsRepo:           terminalsTable,
		DestinationMetadataRepo: &store.DestinationMetadataTable{},
	}

	destinationsIndexHandlerV1 := &handlers.DestinationsIndex{
		ErrorResponse:           errorResponse,
		EgressDestinationStore:  egressDestinationStore,
		EgressDestinationMapper: egressDestinationMapper,
		Logger:                  logger,
	}

	createDestinationsHandlerV1 := &handlers.DestinationsCreate{
		ErrorResponse:           errorResponse,
		EgressDestinationStore:  egressDestinationStore,
		EgressDestinationMapper: egressDestinationMapper,
		Logger:                  logger,
	}

	updateDestinationsHandlerV1 := &handlers.DestinationsUpdate{
		ErrorResponse:           errorResponse,
		EgressDestinationStore:  egressDestinationStore,
		EgressDestinationMapper: egressDestinationMapper,
		Logger:                  logger,
	}

	deleteDestinationHandlerV1 := &handlers.DestinationDelete{
		ErrorResponse:           errorResponse,
		EgressDestinationStore:  egressDestinationStore,
		EgressDestinationMapper: egressDestinationMapper,
		Logger:                  logger,
	}

	egressPolicyValidator := &api.EgressValidator{
		CCClient:         ccClient,
		UAAClient:        uaaClient,
		DestinationStore: egressDestinationStore,
	}

	egressPolicyMapper := &api.EgressPolicyMapper{
		Unmarshaler: marshal.UnmarshalFunc(json.Unmarshal),
		Marshaler:   marshal.MarshalFunc(json.Marshal),
		Validator:   egressPolicyValidator,
	}

	indexEgressPolicyHandlerV1 := &handlers.EgressPolicyIndex{
		ErrorResponse: errorResponse,
		Store:         egressPolicyStore,
		Mapper:        egressPolicyMapper,
		Logger:        logger,
	}

	createEgressPolicyHandlerV1 := &handlers.EgressPolicyCreate{
		Store:         egressPolicyStore,
		Mapper:        egressPolicyMapper,
		ErrorResponse: errorResponse,
		Logger:        logger,
	}

	deleteEgressPolicyHandlerV1 := &handlers.EgressPolicyDelete{
		Store:         egressPolicyStore,
		Mapper:        egressPolicyMapper,
		ErrorResponse: errorResponse,
		Logger:        logger,
	}

	policyCleaner := cleaner.NewPolicyCleaner(logger.Session("policy-cleaner"), wrappedStore, egressPolicyStore, uaaClient,
		ccClient, 100, time.Duration(5)*time.Second)

	policyCollectionWriter := api.NewPolicyCollectionWriter(marshal.MarshalFunc(json.Marshal))
	policiesCleanupHandler := handlers.NewPoliciesCleanup(policyCollectionWriter, policyCleaner, errorResponse)

	tagsIndexHandler := handlers.NewTagsIndex(wrappedStore, marshal.MarshalFunc(json.Marshal), errorResponse)

	healthHandler := handlers.NewHealth(wrappedStore, errorResponse)

	checkVersionWrapper := &handlers.CheckVersionWrapper{
		ErrorResponse: errorResponse,
		RataAdapter:   adapter.RataAdapter{},
	}

	metricsWrap := func(name string, handler http.Handler) http.Handler {
		metricsWrapper := middleware.MetricWrapper{
			Name:          name,
			MetricsSender: metricsSender,
		}
		return metricsWrapper.Wrap(handler)
	}

	logWrapper := middleware.LogWrapper{
		UUIDGenerator: &middlewareAdapter.UUIDAdapter{},
	}

	logWrap := func(handler http.Handler) http.Handler {
		return logWrapper.LogWrap(logger, handler)
	}

	versionWrap := func(v1Handler, v0Handler http.Handler) http.Handler {
		return checkVersionWrapper.CheckVersion(map[string]http.Handler{
			"v1": v1Handler,
			"v0": v0Handler,
		})
	}

	authAdminWrap := func(handler http.Handler) http.Handler {
		networkAdminAuthenticator := handlers.Authenticator{
			Client:        uaaClient,
			Scopes:        []string{"network.admin"},
			ErrorResponse: errorResponse,
			ScopeChecking: true,
		}
		return networkAdminAuthenticator.Wrap(handler)
	}

	authWriteWrap := func(handler http.Handler) http.Handler {
		networkWriteAuthenticator := handlers.Authenticator{
			Client:        uaaClient,
			Scopes:        []string{"network.admin", "network.write"},
			ErrorResponse: errorResponse,
			ScopeChecking: !conf.EnableSpaceDeveloperSelfService,
		}
		return networkWriteAuthenticator.Wrap(handler)
	}

	externalRoutes := rata.Routes{
		{Name: "uptime", Method: "GET", Path: "/"},
		{Name: "uptime", Method: "GET", Path: "/networking"},
		{Name: "health", Method: "GET", Path: "/health"},
		{Name: "whoami", Method: "GET", Path: "/networking/:version/external/whoami"},
		{Name: "create_policies", Method: "POST", Path: "/networking/:version/external/policies"},
		{Name: "delete_policies", Method: "POST", Path: "/networking/:version/external/policies/delete"},
		{Name: "policies_index", Method: "GET", Path: "/networking/:version/external/policies"},
		{Name: "destinations_index", Method: "GET", Path: "/networking/:version/external/destinations"},
		{Name: "destinations_create", Method: "POST", Path: "/networking/:version/external/destinations"},
		{Name: "destinations_update", Method: "PUT", Path: "/networking/:version/external/destinations"},
		{Name: "destination_delete", Method: "DELETE", Path: "/networking/:version/external/destinations/:id"},
		{Name: "egress_policies_index", Method: "GET", Path: "/networking/:version/external/egress_policies"},
		{Name: "egress_policies_create", Method: "POST", Path: "/networking/:version/external/egress_policies"},
		{Name: "egress_policies_delete", Method: "DELETE", Path: "/networking/:version/external/egress_policies/:id"},
		{Name: "cleanup", Method: "POST", Path: "/networking/:version/external/policies/cleanup"},
		{Name: "tags_index", Method: "GET", Path: "/networking/:version/external/tags"},
	}

	corsMiddleware := psmiddleware.CORS{}
	externalRoutesWithOptions := corsMiddleware.AddOptionsRoutes("options", externalRoutes)

	corsOptionsWrapper := func(handler http.Handler) http.Handler {
		wrapper := handlers.CORSOptionsWrapper{
			RataRoutes:         externalRoutesWithOptions,
			AllowedCORSDomains: conf.AllowedCORSDomains,
		}
		return wrapper.Wrap(handler)
	}

	externalHandlers := rata.Handlers{
		"options": corsOptionsWrapper(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		})),
		"uptime": corsOptionsWrapper(metricsWrap("Uptime", logWrap(uptimeHandler))),
		"health": corsOptionsWrapper(metricsWrap("Health", logWrap(healthHandler))),

		"create_policies": corsOptionsWrapper(metricsWrap("CreatePolicies",
			logWrap(versionWrap(authWriteWrap(createPolicyHandlerV1), authWriteWrap(createPolicyHandlerV0))))),

		"delete_policies": corsOptionsWrapper(metricsWrap("DeletePolicies",
			logWrap(versionWrap(authWriteWrap(deletePolicyHandlerV1), authWriteWrap(deletePolicyHandlerV0))))),

		"policies_index": corsOptionsWrapper(metricsWrap("PoliciesIndex",
			logWrap(versionWrap(authWriteWrap(policiesIndexHandlerV1), authWriteWrap(policiesIndexHandlerV0))))),

		"destinations_index": corsOptionsWrapper(metricsWrap("DestinationsIndex",
			logWrap(versionWrap(authAdminWrap(destinationsIndexHandlerV1), authAdminWrap(destinationsIndexHandlerV1))))),

		"destinations_create": corsOptionsWrapper(metricsWrap("DestinationsCreate",
			logWrap(authAdminWrap(createDestinationsHandlerV1)))),

		"destinations_update": corsOptionsWrapper(metricsWrap("DestinationsUpdate",
			logWrap(updateDestinationsHandlerV1))),
		//TODO authAdmin wrap

		"destination_delete": corsOptionsWrapper(metricsWrap("DestinationDelete",
			logWrap(authAdminWrap(deleteDestinationHandlerV1)))),

		"egress_policies_index": corsOptionsWrapper(metricsWrap("EgressPoliciesIndex",
			logWrap(authAdminWrap(indexEgressPolicyHandlerV1)))),

		"egress_policies_create": corsOptionsWrapper(metricsWrap("EgressPoliciesCreate",
			logWrap(authAdminWrap(createEgressPolicyHandlerV1)))),

		"egress_policies_delete": corsOptionsWrapper(metricsWrap("EgressPoliciesDelete",
			logWrap(authAdminWrap(deleteEgressPolicyHandlerV1)))),

		"cleanup": corsOptionsWrapper(metricsWrap("Cleanup",
			logWrap(versionWrap(authAdminWrap(policiesCleanupHandler), authAdminWrap(policiesCleanupHandler))))),

		"tags_index": corsOptionsWrapper(metricsWrap("TagsIndex",
			logWrap(versionWrap(authAdminWrap(tagsIndexHandler), authAdminWrap(tagsIndexHandler))))),

		"whoami": corsOptionsWrapper(metricsWrap("WhoAmI",
			logWrap(versionWrap(authAdminWrap(whoamiHandler), authAdminWrap(whoamiHandler))))),
	}

	err = dropsonde.Initialize(conf.MetronAddress, dropsondeOrigin)
	if err != nil {
		log.Fatalf("%s.%s: initializing dropsonde: %s", logPrefix, jobPrefix, err)
	}

	metricsEmitter := common.InitMetricsEmitter(logger, wrappedStore, connectionPool)
	externalServer := common.InitServer(logger, nil, conf.ListenHost, conf.ListenPort, externalHandlers, externalRoutesWithOptions)
	policyPoller := initPoller(logger, conf, policyCleaner)
	debugServer := debugserver.Runner(fmt.Sprintf("%s:%d", conf.DebugServerHost, conf.DebugServerPort), reconfigurableSink)

	members := grouper.Members{
		{"metrics_emitter", metricsEmitter},
		{"http_server", externalServer},
		{"policy-cleaner-poller", policyPoller},
		{"debug-server", debugServer},
	}

	logger.Info("starting external server", lager.Data{"listen-address": conf.ListenHost, "port": conf.ListenPort})

	group := grouper.NewOrdered(os.Interrupt, members)
	monitor := ifrit.Invoke(sigmon.New(group))

	err = <-monitor.Wait()
	if connectionPool != nil {
		connectionPool.Close()
	}
	if err != nil {
		logger.Error("exited-with-failure", err)
		os.Exit(1)
	}

	logger.Info("exited")
}

func initPoller(logger lager.Logger, conf *config.Config, policyCleaner *cleaner.PolicyCleaner) ifrit.Runner {
	pollInterval := time.Duration(conf.CleanupInterval) * time.Second

	return &poller.Poller{
		Logger:          logger.Session("policy-cleaner-poller"),
		PollInterval:    pollInterval,
		SingleCycleFunc: policyCleaner.DeleteStalePoliciesWrapper,
	}
}
