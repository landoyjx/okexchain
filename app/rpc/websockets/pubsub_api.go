package websockets

import (
	"fmt"
	"sync"

	"github.com/gorilla/websocket"

	"github.com/tendermint/tendermint/libs/log"
	coretypes "github.com/tendermint/tendermint/rpc/core/types"
	tmtypes "github.com/tendermint/tendermint/types"

	"github.com/cosmos/cosmos-sdk/client/context"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/eth/filters"
	"github.com/ethereum/go-ethereum/rpc"

	rpcfilters "github.com/okex/okexchain/app/rpc/namespaces/eth/filters"
	rpctypes "github.com/okex/okexchain/app/rpc/types"
	evmtypes "github.com/okex/okexchain/x/evm/types"
)

// PubSubAPI is the eth_ prefixed set of APIs in the Web3 JSON-RPC spec
type PubSubAPI struct {
	clientCtx context.CLIContext
	events    *rpcfilters.EventSystem
	filtersMu sync.Mutex
	filters   map[rpc.ID]*wsSubscription
	logger    log.Logger
}

// NewAPI creates an instance of the ethereum PubSub API.
func NewAPI(clientCtx context.CLIContext, log log.Logger) *PubSubAPI {
	return &PubSubAPI{
		clientCtx: clientCtx,
		events:    rpcfilters.NewEventSystem(clientCtx.Client),
		filters:   make(map[rpc.ID]*wsSubscription),
		logger:    log.With("module", "websocket-client"),
	}
}

func (api *PubSubAPI) subscribe(conn *websocket.Conn, params []interface{}) (rpc.ID, error) {
	method, ok := params[0].(string)
	if !ok {
		return "0", fmt.Errorf("invalid parameters")
	}

	switch method {
	case "newHeads":
		// TODO: handle extra params
		return api.subscribeNewHeads(conn)
	case "logs":
		if len(params) > 1 {
			return api.subscribeLogs(conn, params[1])
		}

		return api.subscribeLogs(conn, nil)
	case "newPendingTransactions":
		return api.subscribePendingTransactions(conn)
	case "syncing":
		return api.subscribeSyncing(conn)
	default:
		return "0", fmt.Errorf("unsupported method %s", method)
	}
}

func (api *PubSubAPI) unsubscribe(id rpc.ID) bool {
	api.filtersMu.Lock()
	defer api.filtersMu.Unlock()

	if api.filters[id] == nil {
		return false
	}
	if api.filters[id].sub != nil {
		api.filters[id].sub.Unsubscribe(api.events)
	}
	close(api.filters[id].unsubscribed)
	delete(api.filters, id)
	return true
}

func (api *PubSubAPI) subscribeNewHeads(conn *websocket.Conn) (rpc.ID, error) {
	sub, _, err := api.events.SubscribeNewHeads()
	if err != nil {
		return "", fmt.Errorf("error creating block filter: %s", err.Error())
	}

	unsubscribed := make(chan struct{})
	api.filtersMu.Lock()
	api.filters[sub.ID()] = &wsSubscription{
		sub:          sub,
		conn:         conn,
		unsubscribed: unsubscribed,
	}
	api.filtersMu.Unlock()

	go func(headersCh <-chan coretypes.ResultEvent, errCh <-chan error) {
		for {
			select {
			case event := <-headersCh:
				data, _ := event.Data.(tmtypes.EventDataNewBlockHeader)
				headerWithBlockHash, err := rpctypes.EthHeaderWithBlockHashFromTendermint(&data.Header)
				if err != nil {
					api.logger.Error("failed to get header with block hash", err)
					continue
				}

				api.filtersMu.Lock()
				if f, found := api.filters[sub.ID()]; found {
					// write to ws conn
					res := &SubscriptionNotification{
						Jsonrpc: "2.0",
						Method:  "eth_subscription",
						Params: &SubscriptionResult{
							Subscription: sub.ID(),
							Result:       headerWithBlockHash,
						},
					}

					err = f.conn.WriteJSON(res)
					if err != nil {
						api.logger.Error("error writing header")
					}
				}
				api.filtersMu.Unlock()

				if err == websocket.ErrCloseSent {
					api.unsubscribe(sub.ID())
				}
			case <-errCh:
				api.filtersMu.Lock()
				sub.Unsubscribe(api.events)
				delete(api.filters, sub.ID())
				api.filtersMu.Unlock()
				return
			case <-unsubscribed:
				return
			}
		}
	}(sub.Event(), sub.Err())

	return sub.ID(), nil
}

func (api *PubSubAPI) subscribeLogs(conn *websocket.Conn, extra interface{}) (rpc.ID, error) {
	crit := filters.FilterCriteria{}

	if extra != nil {
		params, ok := extra.(map[string]interface{})
		if !ok {
			return "", fmt.Errorf("invalid criteria")
		}

		if params["address"] != nil {
			address, ok := params["address"].(string)
			addresses, sok := params["address"].([]interface{})
			if !ok && !sok {
				return "", fmt.Errorf("invalid address; must be address or array of addresses")
			}

			if ok {
				if !common.IsHexAddress(address) {
					return "", fmt.Errorf("invalid address")
				}
				crit.Addresses = []common.Address{common.HexToAddress(address)}
			} else if sok {
				crit.Addresses = []common.Address{}
				for _, addr := range addresses {
					address, ok := addr.(string)
					if !ok || !common.IsHexAddress(address) {
						return "", fmt.Errorf("invalid address")
					}

					crit.Addresses = append(crit.Addresses, common.HexToAddress(address))
				}
			}
		}

		if params["topics"] != nil {
			topics, ok := params["topics"].([]interface{})
			if !ok {
				return "", fmt.Errorf("invalid topics")
			}

			topicFilterLists, err := resolveTopicList(topics)
			if err != nil {
				return "", fmt.Errorf("invalid topics")
			}
			crit.Topics = topicFilterLists
		}
	}

	sub, _, err := api.events.SubscribeLogs(crit)
	if err != nil {
		return rpc.ID(""), err
	}

	unsubscribed := make(chan struct{})
	api.filtersMu.Lock()
	api.filters[sub.ID()] = &wsSubscription{
		sub:          sub,
		conn:         conn,
		unsubscribed: unsubscribed,
	}
	api.filtersMu.Unlock()

	go func(ch <-chan coretypes.ResultEvent, errCh <-chan error) {
		for {
			select {
			case event := <-ch:
				dataTx, ok := event.Data.(tmtypes.EventDataTx)
				if !ok {
					err = fmt.Errorf("invalid event data %T, expected EventDataTx", event.Data)
					return
				}

				var resultData evmtypes.ResultData
				resultData, err = evmtypes.DecodeResultData(dataTx.TxResult.Result.Data)
				if err != nil {
					return
				}

				logs := rpcfilters.FilterLogs(resultData.Logs, crit.FromBlock, crit.ToBlock, crit.Addresses, crit.Topics)

				api.filtersMu.Lock()
				if f, found := api.filters[sub.ID()]; found {
					// write to ws conn
					res := &SubscriptionNotification{
						Jsonrpc: "2.0",
						Method:  "eth_subscription",
						Params: &SubscriptionResult{
							Subscription: sub.ID(),
						},
					}
					for _, singleLog := range logs {
						res.Params.Result = singleLog
						err = f.conn.WriteJSON(res)
						if err != nil {
							api.logger.Error(fmt.Sprintf("failed to write header: %s", err))
							break
						}
					}
				}
				api.filtersMu.Unlock()

				if err == websocket.ErrCloseSent {
					api.unsubscribe(sub.ID())
				}
			case <-errCh:
				api.filtersMu.Lock()
				sub.Unsubscribe(api.events)
				delete(api.filters, sub.ID())
				api.filtersMu.Unlock()
				return
			case <-unsubscribed:
				return
			}
		}
	}(sub.Event(), sub.Err())

	return sub.ID(), nil
}

func resolveTopicList(params []interface{}) ([][]common.Hash, error) {
	topicFilterLists := make([][]common.Hash, len(params))
	for i, param := range params { // eg: ["0xddf252......f523b3ef", null, ["0x000000......32fea9e4", "0x000000......ab14dc5d"]]
		if param == nil {
			// 1.1 if the topic is null
			topicFilterLists[i] = nil
		} else {
			// 2.1 judge if the param is the type of string or not
			topicStr, ok := param.(string)
			// 2.1 judge if the param is the type of string slice or not
			topicSlices, sok := param.([]interface{})
			if !ok && !sok {
				// if both judgement are false, return invalid topics
				return topicFilterLists, fmt.Errorf("invalid topics")
			}

			if ok {
				// 2.2 This is string
				// 2.3 judge the topic is a valid hex hash or not
				if !IsHexHash(topicStr) {
					return topicFilterLists, fmt.Errorf("invalid topics")
				}
				// 2.4 add this topic to topic-hash-lists
				topicHash := common.HexToHash(topicStr)
				topicFilterLists[i] = []common.Hash{topicHash}
			} else if sok {
				// 2.2 This is slice of string
				topicHashes := make([]common.Hash, len(topicSlices))
				for n, topicStr := range topicSlices {
					//2.3 judge every topic
					topicHash, ok := topicStr.(string)
					if !ok || !IsHexHash(topicHash) {
						return topicFilterLists, fmt.Errorf("invalid topics")
					}
					topicHashes[n] = common.HexToHash(topicHash)
				}
				// 2.4 add this topic slice to topic-hash-lists
				topicFilterLists[i] = topicHashes
			}
		}
	}
	return topicFilterLists, nil
}

func IsHexHash(s string) bool {
	if has0xPrefix(s) {
		s = s[2:]
	}
	return len(s) == 2*common.HashLength && isHex(s)
}

// has0xPrefix validates str begins with '0x' or '0X'.
func has0xPrefix(str string) bool {
	return len(str) >= 2 && str[0] == '0' && (str[1] == 'x' || str[1] == 'X')
}

// isHexCharacter returns bool of c being a valid hexadecimal.
func isHexCharacter(c byte) bool {
	return ('0' <= c && c <= '9') || ('a' <= c && c <= 'f') || ('A' <= c && c <= 'F')
}

// isHex validates whether each byte is valid hexadecimal string.
func isHex(str string) bool {
	if len(str)%2 != 0 {
		return false
	}
	for _, c := range []byte(str) {
		if !isHexCharacter(c) {
			return false
		}
	}
	return true
}

func (api *PubSubAPI) subscribePendingTransactions(conn *websocket.Conn) (rpc.ID, error) {
	sub, _, err := api.events.SubscribePendingTxs()
	if err != nil {
		return "", fmt.Errorf("error creating block filter: %s", err.Error())
	}

	unsubscribed := make(chan struct{})
	api.filtersMu.Lock()
	api.filters[sub.ID()] = &wsSubscription{
		sub:          sub,
		conn:         conn,
		unsubscribed: unsubscribed,
	}
	api.filtersMu.Unlock()

	go func(txsCh <-chan coretypes.ResultEvent, errCh <-chan error) {
		for {
			select {
			case ev := <-txsCh:
				data, _ := ev.Data.(tmtypes.EventDataTx)
				txHash := common.BytesToHash(data.Tx.Hash())

				api.filtersMu.Lock()
				if f, found := api.filters[sub.ID()]; found {
					// write to ws conn
					res := &SubscriptionNotification{
						Jsonrpc: "2.0",
						Method:  "eth_subscription",
						Params: &SubscriptionResult{
							Subscription: sub.ID(),
							Result:       txHash,
						},
					}

					err = f.conn.WriteJSON(res)
					if err != nil {
						api.logger.Error(fmt.Sprintf("failed to write header: %s", err.Error()))
					}
				}
				api.filtersMu.Unlock()

				if err == websocket.ErrCloseSent {
					api.unsubscribe(sub.ID())
				}
			case <-errCh:
				api.filtersMu.Lock()
				sub.Unsubscribe(api.events)
				delete(api.filters, sub.ID())
				api.filtersMu.Unlock()
				return
			case <-unsubscribed:
				return
			}
		}
	}(sub.Event(), sub.Err())

	return sub.ID(), nil
}

func (api *PubSubAPI) subscribeSyncing(conn *websocket.Conn) (rpc.ID, error) {
	sub, _, err := api.events.SubscribeNewHeads()
	if err != nil {
		return "", fmt.Errorf("error creating block filter: %s", err.Error())
	}

	unsubscribed := make(chan struct{})
	api.filtersMu.Lock()
	api.filters[sub.ID()] = &wsSubscription{
		sub:          sub,
		conn:         conn,
		unsubscribed: unsubscribed,
	}
	api.filtersMu.Unlock()

	status, err := api.clientCtx.Client.Status()
	if err != nil {
		return "", fmt.Errorf("error get sync status: %s", err.Error())
	}
	startingBlock := hexutil.Uint64(status.SyncInfo.EarliestBlockHeight)
	highestBlock := hexutil.Uint64(0)

	var result interface{}

	go func(headersCh <-chan coretypes.ResultEvent, errCh <-chan error) {
		for {
			select {
			case <-headersCh:

				newStatus, err := api.clientCtx.Client.Status()
				if err != nil {
					api.logger.Error("error get sync status: %s", err.Error())
				}

				if !newStatus.SyncInfo.CatchingUp {
					result = false
				} else {
					result = map[string]interface{}{
						"startingBlock": startingBlock,
						"currentBlock":  hexutil.Uint64(newStatus.SyncInfo.LatestBlockHeight),
						"highestBlock":  highestBlock,
					}
				}

				api.filtersMu.Lock()
				if f, found := api.filters[sub.ID()]; found {
					// write to ws conn
					res := &SubscriptionNotification{
						Jsonrpc: "2.0",
						Method:  "eth_subscription",
						Params: &SubscriptionResult{
							Subscription: sub.ID(),
							Result:       result,
						},
					}

					err = f.conn.WriteJSON(res)
					if err != nil {
						api.logger.Error("error writing syncing")
					}
				}
				api.filtersMu.Unlock()

				if err == websocket.ErrCloseSent {
					api.unsubscribe(sub.ID())
				}

			case <-errCh:
				api.filtersMu.Lock()
				sub.Unsubscribe(api.events)
				delete(api.filters, sub.ID())
				api.filtersMu.Unlock()
				return
			case <-unsubscribed:
				return
			}
		}
	}(sub.Event(), sub.Err())

	return sub.ID(), nil
}
