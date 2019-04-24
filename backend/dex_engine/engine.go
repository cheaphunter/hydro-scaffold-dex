package dex_engine

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/HydroProtocol/hydro-box-dex/backend/models"
	"github.com/HydroProtocol/hydro-sdk-backend/common"
	"github.com/HydroProtocol/hydro-sdk-backend/config"
	"github.com/HydroProtocol/hydro-sdk-backend/connection"
	"github.com/HydroProtocol/hydro-sdk-backend/engine"
	"github.com/HydroProtocol/hydro-sdk-backend/sdk/ethereum"
	"github.com/HydroProtocol/hydro-sdk-backend/utils"
	"strings"
	"sync"
)

type RedisOrderBookSnapshotHandler struct {
	kvStore common.IKVStore
}

func (handler RedisOrderBookSnapshotHandler) Update(key string, bookSnapshot *common.SnapshotV2) sync.WaitGroup {
	bts, err := json.Marshal(bookSnapshot)
	if err != nil {
		panic(err)
	}

	_ = handler.kvStore.Set(key, string(bts), 0)

	return sync.WaitGroup{}
}

type RedisOrderBookActivitiesHandler struct {
}

func (handler RedisOrderBookActivitiesHandler) Update(webSocketMessages []common.WebSocketMessage) sync.WaitGroup {
	for _, msg := range webSocketMessages {
		if strings.HasPrefix(msg.ChannelID, "Market#") {
			pushMessage(msg)
		}
	}

	return sync.WaitGroup{}
}

type DexEngine struct {
	// global ctx, if this ctx is canceled, queue handlers should exit in a short time.
	ctx context.Context

	// all redis queues handlers
	marketHandlerMap map[string]*MarketHandler
	eventQueue       common.IQueue

	// Wait for all queue handler exit gracefully
	Wg sync.WaitGroup

	HydroEngine *engine.Engine
}

func NewDexEngine(ctx context.Context) *DexEngine {
	// init redis
	redis := connection.NewRedisClient(config.Getenv("HSK_REDIS_URL"))

	// init websocket queue
	wsQueue, _ := common.InitQueue(
		&common.RedisQueueConfig{
			Name:   common.HYDRO_WEBSOCKET_MESSAGES_QUEUE_KEY,
			Ctx:    ctx,
			Client: redis,
		},
	)
	InitWsQueue(wsQueue)

	// init event queue
	eventQueue, _ := common.InitQueue(
		&common.RedisQueueConfig{
			Name:   common.HYDRO_ENGINE_EVENTS_QUEUE_KEY,
			Client: redis,
			Ctx:    ctx,
		})

	e := engine.NewEngine(context.Background())

	// setup handler for hydro engine
	kvStore, _ := common.InitKVStore(&common.RedisKVStoreConfig{Ctx: ctx, Client: redis})
	snapshotHandler := RedisOrderBookSnapshotHandler{kvStore: kvStore}
	e.RegisterOrderBookSnapshotHandler(snapshotHandler)

	activityHandler := RedisOrderBookActivitiesHandler{}
	e.RegisterOrderBookActivitiesHandler(activityHandler)

	engine := &DexEngine{
		ctx:              ctx,
		eventQueue:       eventQueue,
		marketHandlerMap: make(map[string]*MarketHandler),
		Wg:               sync.WaitGroup{},

		HydroEngine: e,
	}

	markets := models.MarketDao.FindAllMarkets()
	for _, market := range markets {
		_, err := engine.newMarket(market.ID)
		if err != nil {
			panic(err)
		}
	}

	return engine
}

func (e *DexEngine) newMarket(marketId string) (marketHandler *MarketHandler, err error) {
	market := models.MarketDao.FindMarketByID(marketId)
	if market == nil {
		err = fmt.Errorf("open market fail, market [%s] not found", marketId)
		return
	}

	marketHandler, err = NewMarketHandler(e.ctx, market, e.HydroEngine)
	if err != nil {
		return
	}

	e.marketHandlerMap[market.ID] = marketHandler
	utils.Info("market %s init done", marketHandler.market.ID)
	return
}

func (e *DexEngine) closeMarket(marketId string) {
	_, ok := e.marketHandlerMap[marketId]
	if !ok {
		utils.Error("close market fail, market [%s] not found", marketId)
		return
	}

	marketHandler := e.marketHandlerMap[marketId]
	delete(e.marketHandlerMap, marketId)
	marketHandler.Stop()
	return
}

func runMarket(e *DexEngine, marketHandler *MarketHandler) {
	e.Wg.Add(1)

	go func() {
		defer e.Wg.Done()

		utils.Info("%s market handler is running", marketHandler.market.ID)
		defer utils.Info("%s market handler is stopped", marketHandler.market.ID)

		marketHandler.Run()
	}()
}

func (e *DexEngine) start() {
	for i := range e.marketHandlerMap {
		marketHandler := e.marketHandlerMap[i]
		runMarket(e, marketHandler)
	}

	go func() {
		for {
			select {
			case <-e.ctx.Done():
				for _, handler := range e.marketHandlerMap {
					close(handler.eventChan)
				}
				return
			default:
				data, err := e.eventQueue.Pop()
				if err != nil {
					panic(err)
				}
				var event common.Event
				err = json.Unmarshal(data, &event)
				if err != nil {
					utils.Error("wrong event format: %+v", err)
					continue
				}

				switch event.Type {
				case common.EventOpenMarket:
					marketHandler, err := e.newMarket(event.MarketID)
					if err == nil {
						runMarket(e, marketHandler)
					} else {
						utils.Error(err.Error())
					}
					break
				case common.EventCloseMarket:
					e.closeMarket(event.MarketID)
					break
				default:
					marketHandler, ok := e.marketHandlerMap[event.MarketID]
					if !ok {
						utils.Error("engine not support market [%s]", event.MarketID)
					}
					marketHandler.eventChan <- data
				}
			}
		}
	}()
}

var hydroProtocol = &ethereum.EthereumHydroProtocol{}

func Run(ctx context.Context, startMetrics func()) {
	utils.Info("dex engine start...")

	//init database
	models.Connect(config.Getenv("HSK_DATABASE_URL"))

	//start dex engine
	dexEngine := NewDexEngine(ctx)
	dexEngine.start()
	go startMetrics()

	dexEngine.Wg.Wait()
	utils.Info("dex engine stopped!")
}
