package sync_greedy_parser

import (
	"context"
	"errors"
	ethereum_jsonrpc "github.com/bluntenpassant/ethereum_subscriber/internal/app/client/ethereum-jsonrpc"
	ethereum_jsonrpc_models "github.com/bluntenpassant/ethereum_subscriber/internal/app/client/ethereum-jsonrpc/models"
	"github.com/bluntenpassant/ethereum_subscriber/internal/app/models"
	"sync"
)

type EthereumJsonRPCClient interface {
	GetTxCount(req *ethereum_jsonrpc.GetTxCountReq) (*ethereum_jsonrpc.GetTxCountResp, error)
	GetBlockByNumber(req *ethereum_jsonrpc.GetBlockByNumberReq) (*ethereum_jsonrpc.GetBlockByNumberResp, error)
	GetBlockNumber() (*ethereum_jsonrpc.GetBlockNumberResp, error)
}

type SubscriberRepository interface {
	GetTransactionsReversed(ctx context.Context, address string) ([]*models.Transaction, error)
	AddNewSubscriber(ctx context.Context, subscriber models.Subscriber) error
	GetSubscriberByAddress(ctx context.Context, address string) (models.Subscriber, error)
	GetLastTransaction(ctx context.Context, address string) (*models.Transaction, error)
	AddTransactions(ctx context.Context, address string, txs []*models.Transaction) error
}

type BlockRepository interface {
	SetMaxCurrentBlock(ctx context.Context, newCurrentBlock uint64) error
	GetCurrentBlock(ctx context.Context) (uint64, error)
}

type Parser struct {
	ethereumJsonRPCClient EthereumJsonRPCClient
	subscriberRepository  SubscriberRepository
	blockRepository       BlockRepository
}

func NewParser(ethereumJsonRPCClient EthereumJsonRPCClient, subscriberRepository SubscriberRepository, blockRepository BlockRepository) *Parser {
	return &Parser{
		ethereumJsonRPCClient: ethereumJsonRPCClient,
		subscriberRepository:  subscriberRepository,
		blockRepository:       blockRepository,
	}
}

func (p *Parser) GetCurrentBlock(ctx context.Context) (uint64, error) {
	currentBlock, err := p.blockRepository.GetCurrentBlock(ctx)

	return currentBlock, err
}

func (p *Parser) Subscribe(ctx context.Context, address string) error {
	blockNumberResp, err := p.ethereumJsonRPCClient.GetBlockNumber()
	if err != nil {
		return errors.New("error getting current block number cause: " + err.Error())
	}

	txCountResp, err := p.ethereumJsonRPCClient.GetTxCount(&ethereum_jsonrpc.GetTxCountReq{
		Address:  address,
		EndBlock: blockNumberResp.BlockNumber,
	})
	if err != nil {
		return err
	}

	err = p.subscriberRepository.AddNewSubscriber(ctx, models.Subscriber{
		Address:              address,
		SubscribeBlockNumber: uint64(blockNumberResp.BlockNumber),
		SubscribeTxCount:     uint64(txCountResp.Nonce),
	})

	return err
}

// GetTransactions uses for a getting a full list of inbound or outbounds transactions by user since subscription.
// Due to simple transactions in Ethereum Network are not indexed, we cannot get it through simple JSONRPC methods.
// This method uses heuristic approach without having to process the entire chain.
// Algorithm:
// 1. Get current block number in ethereum network to set up point to which we must search and block number in a moment of last parsed block (currentBlockNumber, lastBlockNumber).
// 2. Get count of inbound or outbound transactions in current moment and in a moment of last parsed txCount related to user (currentTxCount, lastTxCount)
// 3. txCount (count of transactions that should be handled in a future starting from last parsed time) = currentTxCount - lastTxCount
// 4. Iterate in range currentBlockNumber..lastBlockNumber and get all transactions for every block where from==address or to==address until txCount > 0
// NOTE 1*: method starting loop from currentBlockNumber to lastBlockNumber, because we have a gap from currentBlockNumber,
// because we could start our subscription not from start transaction for block
// Example: transaction count in every block = 3 (blockSize), blockCount = 10, currentBlockNumber = 10,
// lastBlockNumber = 1 (1-indexed block numbers in this example), lastTxCount = 2, currentTxCount = 28
// txCount = 26, if we iterate from 1 block and 1 transaction then we will catch transaction 1..27 instead of 3..29,
// so we need to start from transaction 3 (subscriptionTxCount + 1), but if we had a lastTxCount > blockSize
// then we need to find our starting transaction first, it is a little annoying
// it is much easier to iterate from last block because we will already have reversed order, and we don`t need to count transactions
// NOTE 2*: as opposed to SyncParser (Releasing Approach) approach Greedy implies saving already parsed transaction,
// we can afford it due to Ethereum transaction immutability promise (https://ethereum.org/en/developers/docs/transactions/#:~:text=As%20time%20passes,billions%20of%20dollars.)
// this method might require distributed storage (like Redis or PostgreSQL) instead of default memory storage
// but it might significantly increase performance due to keeping already parsed transactions.
// This approach aimed at long-term program execution with long lifetime.
func (p *Parser) GetTransactions(ctx context.Context, address string) ([]*models.Transaction, error) {
	subscriber, err := p.subscriberRepository.GetSubscriberByAddress(ctx, address)
	if err != nil {
		return nil, err
	}

	currentBlockNumberResp, err := p.ethereumJsonRPCClient.GetBlockNumber()
	if err != nil {
		return nil, err
	}

	currentTxCountResp, err := p.ethereumJsonRPCClient.GetTxCount(&ethereum_jsonrpc.GetTxCountReq{
		Address:  address,
		EndBlock: currentBlockNumberResp.BlockNumber,
	})
	if err != nil {
		return nil, err
	}

	txCount := uint64(currentTxCountResp.Nonce) - subscriber.SubscribeTxCount

	transactions := make([]*models.Transaction, 0, txCount)

	var txPool = sync.Pool{
		New: func() interface{} {
			return &models.Transaction{}
		},
	}

	lastTx, err := p.subscriberRepository.GetLastTransaction(ctx, address)
	if err != nil {
		return nil, err
	}

	for i := uint64(currentBlockNumberResp.BlockNumber); i >= subscriber.SubscribeBlockNumber && (txCount > 0); i-- {
		blockNumber := i

		blockResp, err := p.ethereumJsonRPCClient.GetBlockByNumber(&ethereum_jsonrpc.GetBlockByNumberReq{
			BlockNumber: ethereum_jsonrpc_models.HexUint64(blockNumber),
			IsGetFullTx: true,
		})
		if err != nil {
			return nil, err
		}

		if blockNumber == uint64(currentBlockNumberResp.BlockNumber) {
			err = p.blockRepository.SetMaxCurrentBlock(ctx, blockNumber)
			if err != nil {
				return nil, err
			}
		}

		for j := len(blockResp.Block.Transactions) - 1; j >= 0 && txCount > 0; j-- {
			tx := blockResp.Block.Transactions[j]
			if i == subscriber.SubscribeBlockNumber && lastTx != nil && (uint64(tx.TransactionIndex) < lastTx.TransactionIndex) {
				continue
			}
			if tx.From == address || tx.To == address {
				transaction := txPool.Get().(*models.Transaction)
				*transaction = *models.ConvertJsonRPCTxToInternal(tx)
				transactions = append(transactions, transaction)

				txCount--
			}
		}
	}

	models.ReverseTransactionsByLink(transactions)

	err = p.subscriberRepository.AddTransactions(ctx, address, transactions)
	if err != nil {
		return nil, err
	}

	sharedTransactions, err := p.subscriberRepository.GetTransactionsReversed(ctx, address)

	return sharedTransactions, nil
}
