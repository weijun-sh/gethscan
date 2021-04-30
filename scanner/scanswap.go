package scanner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"sync"
	"time"

	"github.com/anyswap/CrossChain-Bridge/cmd/utils"
	"github.com/anyswap/CrossChain-Bridge/log"
	"github.com/anyswap/CrossChain-Bridge/rpc/client"
	"github.com/anyswap/CrossChain-Bridge/tokens"
	ctools "github.com/anyswap/CrossChain-Bridge/tokens/tools"
	"github.com/fsn-dev/fsn-go-sdk/efsn/common"
	"github.com/fsn-dev/fsn-go-sdk/efsn/core/types"
	"github.com/fsn-dev/fsn-go-sdk/efsn/ethclient"
	"github.com/jowenshaw/gethscan/params"
	"github.com/jowenshaw/gethscan/tools"
	"github.com/urfave/cli/v2"
)

var (
	scanReceiptFlag = &cli.BoolFlag{
		Name:  "scanReceipt",
		Usage: "scan transaction receipt instead of transaction",
	}

	startHeightFlag = &cli.Int64Flag{
		Name:  "start",
		Usage: "start height (start inclusive)",
		Value: -200,
	}

	// ScanSwapCommand scan swaps on eth like blockchain
	ScanSwapCommand = &cli.Command{
		Action:    scanSwap,
		Name:      "scanswap",
		Usage:     "scan cross chain swaps",
		ArgsUsage: " ",
		Description: `
scan cross chain swaps
`,
		Flags: []cli.Flag{
			utils.ConfigFileFlag,
			utils.GatewayFlag,
			scanReceiptFlag,
			startHeightFlag,
			utils.EndHeightFlag,
			utils.StableHeightFlag,
			utils.JobsFlag,
		},
	}

	transferFuncHash       = common.FromHex("0xa9059cbb")
	transferFromFuncHash   = common.FromHex("0x23b872dd")
	addressSwapoutFuncHash = common.FromHex("0x628d6cba") // for ETH like `address` type address
	stringSwapoutFuncHash  = common.FromHex("0xad54056d") // for BTC like `string` type address

	transferLogTopic       = common.HexToHash("0xddf252ad1be2c89b69c2b068fc378daa952ba7f163c4a11628f55a4df523b3ef")
	addressSwapoutLogTopic = common.HexToHash("0x6b616089d04950dc06c45c6dd787d657980543f89651aec47924752c7d16c888")
	stringSwapoutLogTopic  = common.HexToHash("0x9c92ad817e5474d30a4378deface765150479363a897b0590fbb12ae9d89396b")
)

const (
	txSwapin   = "swapin"
	txSwapout  = "swapout"
	txSwapout2 = "swapout2"

	swapExistKeywords   = "mgoError: Item is duplicate"
	httpTimeoutKeywords = "Client.Timeout exceeded while awaiting headers"
)

var startHeightArgument int64

type ethSwapScanner struct {
	gateway     string
	scanReceipt bool

	endHeight    uint64
	stableHeight uint64
	jobCount     uint64

	client *ethclient.Client
	ctx    context.Context

	rpcInterval   time.Duration
	rpcRetryCount int

	cachedSwapPosts *tools.Ring
}

type swapPost struct {
	txid       string
	pairID     string
	rpcMethod  string
	swapServer string
}

func scanSwap(ctx *cli.Context) error {
	utils.SetLogger(ctx)
	params.LoadConfig(utils.GetConfigFilePath(ctx))
	go params.WatchAndReloadScanConfig()

	scanner := &ethSwapScanner{
		ctx:           context.Background(),
		rpcInterval:   1 * time.Second,
		rpcRetryCount: 3,
	}
	scanner.gateway = ctx.String(utils.GatewayFlag.Name)
	scanner.scanReceipt = ctx.Bool(scanReceiptFlag.Name)
	startHeightArgument = ctx.Int64(startHeightFlag.Name)
	scanner.endHeight = ctx.Uint64(utils.EndHeightFlag.Name)
	scanner.stableHeight = ctx.Uint64(utils.StableHeightFlag.Name)
	scanner.jobCount = ctx.Uint64(utils.JobsFlag.Name)

	log.Info("get argument success",
		"gateway", scanner.gateway,
		"scanReceipt", scanner.scanReceipt,
		"start", startHeightArgument,
		"end", scanner.endHeight,
		"stable", scanner.stableHeight,
		"jobs", scanner.jobCount,
	)

	scanner.initClient()
	scanner.run()
	return nil
}

func (scanner *ethSwapScanner) initClient() {
	ethcli, err := ethclient.Dial(scanner.gateway)
	if err != nil {
		log.Fatal("ethclient.Dail failed", "gateway", scanner.gateway, "err", err)
	}
	log.Info("ethclient.Dail gateway success", "gateway", scanner.gateway)
	scanner.client = ethcli
}

func (scanner *ethSwapScanner) run() {
	scanner.cachedSwapPosts = tools.NewRing(100)
	go scanner.repostCachedSwaps()

	wend := scanner.endHeight
	if wend == 0 {
		wend = scanner.loopGetLatestBlockNumber()
	}
	if startHeightArgument != 0 {
		var start uint64
		if startHeightArgument > 0 {
			start = uint64(startHeightArgument)
		} else if startHeightArgument < 0 {
			start = wend - uint64(-startHeightArgument)
		}
		scanner.doScanRangeJob(start, wend)
	}
	if scanner.endHeight == 0 {
		scanner.scanLoop(wend)
	}
}

func (scanner *ethSwapScanner) doScanRangeJob(start, end uint64) {
	log.Info("start scan range job", "start", start, "end", end, "jobs", scanner.jobCount)
	if scanner.jobCount == 0 {
		log.Fatal("zero count jobs specified")
	}
	if start >= end {
		log.Fatalf("wrong scan range [%v, %v)", start, end)
	}
	jobs := scanner.jobCount
	count := end - start
	step := count / jobs
	if step == 0 {
		jobs = 1
		step = count
	}
	wg := new(sync.WaitGroup)
	for i := uint64(0); i < jobs; i++ {
		from := start + i*step
		to := start + (i+1)*step
		if i+1 == jobs {
			to = end
		}
		wg.Add(1)
		go scanner.scanRange(i+1, from, to, wg)
	}
	if scanner.endHeight != 0 {
		wg.Wait()
	}
}

func (scanner *ethSwapScanner) scanRange(job, from, to uint64, wg *sync.WaitGroup) {
	defer wg.Done()
	log.Info(fmt.Sprintf("[%v] scan range", job), "from", from, "to", to)

	for h := from; h < to; h++ {
		scanner.scanBlock(job, h, false)
	}

	log.Info(fmt.Sprintf("[%v] scan range finish", job), "from", from, "to", to)
}

func (scanner *ethSwapScanner) scanLoop(from uint64) {
	stable := scanner.stableHeight
	log.Info("start scan loop job", "from", from, "stable", stable)
	for {
		latest := scanner.loopGetLatestBlockNumber()
		for h := latest; h > from; h-- {
			scanner.scanBlock(0, h, true)
		}
		if from+stable < latest {
			from = latest - stable
		}
		time.Sleep(5 * time.Second)
	}
}

func (scanner *ethSwapScanner) loopGetLatestBlockNumber() uint64 {
	for {
		header, err := scanner.client.HeaderByNumber(scanner.ctx, nil)
		if err == nil {
			log.Info("get latest block number success", "height", header.Number)
			return header.Number.Uint64()
		}
		log.Warn("get latest block number failed", "err", err)
		time.Sleep(scanner.rpcInterval)
	}
}

func (scanner *ethSwapScanner) loopGetTxReceipt(txHash common.Hash) (receipt *types.Receipt, err error) {
	for i := 0; i < 3; i++ { // with retry
		receipt, err = scanner.client.TransactionReceipt(scanner.ctx, txHash)
		if err == nil {
			return receipt, err
		}
		time.Sleep(scanner.rpcInterval)
	}
	return nil, err
}

func (scanner *ethSwapScanner) loopGetBlock(height uint64) (block *types.Block, err error) {
	blockNumber := new(big.Int).SetUint64(height)
	for i := 0; i < 3; i++ {
		block, err = scanner.client.BlockByNumber(scanner.ctx, blockNumber)
		if err == nil {
			return block, nil
		}
		log.Warn("get block failed", "height", height, "err", err)
		time.Sleep(scanner.rpcInterval)
	}
	return nil, err
}

func (scanner *ethSwapScanner) scanBlock(job, height uint64, cache bool) {
	block, err := scanner.loopGetBlock(height)
	if err != nil {
		return
	}
	blockHash := block.Hash().Hex()
	if cache && cachedBlocks.isScanned(blockHash) {
		return
	}
	log.Info(fmt.Sprintf("[%v] scan block %v", job, height), "hash", blockHash, "txs", len(block.Transactions()))
	for _, tx := range block.Transactions() {
		scanner.scanTransaction(tx)
	}
	if cache {
		cachedBlocks.addBlock(blockHash)
	}
}

func (scanner *ethSwapScanner) scanTransaction(tx *types.Transaction) {
	if tx.To() == nil {
		return
	}
	txHash := tx.Hash().Hex()
	var receipt *types.Receipt
	if scanner.scanReceipt {
		r, err := scanner.loopGetTxReceipt(tx.Hash())
		if err != nil {
			log.Warn("get tx receipt error", "txHash", txHash, "err", err)
			return
		}
		receipt = r
	}

	for _, tokenCfg := range params.GetScanConfig().Tokens {
		matched, verifyErr := scanner.verifyTransaction(tx, receipt, tokenCfg)
		if matched {
			if tokens.ShouldRegisterSwapForError(verifyErr) {
				scanner.postSwap(txHash, tokenCfg)
			} else {
				scanner.printVerifyError(txHash, verifyErr)
			}
			break
		}
	}
}

func (scanner *ethSwapScanner) verifyTransaction(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (matched bool, verifyErr error) {
	txTo := tx.To().Hex()
	tokenAddress := tokenCfg.TokenAddress
	depositAddress := tokenCfg.DepositAddress

	if tokenCfg.VerifyByReceipt && receipt == nil {
		txHash := tx.Hash()
		r, err := scanner.loopGetTxReceipt(txHash)
		if err != nil {
			log.Warn("get tx receipt error", "txHash", txHash, "err", err)
			return false, nil
		}
		receipt = r
	}

	switch {
	case depositAddress != "":
		if tokenCfg.IsNativeToken() {
			matched = strings.EqualFold(txTo, depositAddress)
			return matched, nil
		} else if strings.EqualFold(txTo, tokenAddress) {
			verifyErr = scanner.verifyErc20SwapinTx(tx, receipt, tokenCfg)
			if verifyErr == tokens.ErrTxWithWrongReceiver {
				// swapin my have multiple deposit addresses for different bridges
				return false, verifyErr
			}
			return true, verifyErr
		}
	case !scanner.scanReceipt:
		if strings.EqualFold(txTo, tokenAddress) {
			verifyErr = scanner.verifySwapoutTx(tx, receipt, tokenCfg)
			return true, verifyErr
		}
	default:
		verifyErr = scanner.parseSwapoutTxLogs(receipt.Logs, tokenAddress, tokenCfg.TxType)
		if verifyErr == nil {
			return true, nil
		}
	}
	return false, verifyErr
}

func (scanner *ethSwapScanner) printVerifyError(txHash string, verifyErr error) {
	switch {
	case errors.Is(verifyErr, tokens.ErrTxFuncHashMismatch):
	case errors.Is(verifyErr, tokens.ErrTxWithWrongReceiver):
	case errors.Is(verifyErr, tokens.ErrTxWithWrongContract):
	case errors.Is(verifyErr, tokens.ErrTxNotFound):
	default:
		log.Debug("verify swap error", "txHash", txHash, "err", verifyErr)
	}
}

func (scanner *ethSwapScanner) postSwap(txid string, tokenCfg *params.TokenConfig) {
	pairID := tokenCfg.PairID
	var subject, rpcMethod string
	if tokenCfg.DepositAddress != "" {
		subject = "post swapin register"
		rpcMethod = "swap.Swapin"
	} else {
		subject = "post swapout register"
		rpcMethod = "swap.Swapout"
	}
	log.Info(subject, "txid", txid, "pairID", pairID)

	swap := &swapPost{
		txid:       txid,
		pairID:     pairID,
		rpcMethod:  rpcMethod,
		swapServer: tokenCfg.SwapServer,
	}

	var needCached bool
	for i := 0; i < scanner.rpcRetryCount; i++ {
		err := rpcPost(swap)
		if tokens.ShouldRegisterSwapForError(err) {
			needCached = false
			break
		}
		if ctools.IsSwapAlreadyExistRegisterError(err) ||
			strings.Contains(err.Error(), swapExistKeywords) {
			needCached = false
			break
		}
		switch {
		case errors.Is(err, tokens.ErrTxFuncHashMismatch):
			break
		case errors.Is(err, tokens.ErrTxNotFound) ||
			strings.Contains(err.Error(), httpTimeoutKeywords):
			needCached = true
		default:
			log.Warn(subject+" failed", "swap", swap, "err", err)
		}
		time.Sleep(scanner.rpcInterval)
	}
	if needCached {
		log.Warn("cache swap", "swap", swap)
		scanner.cachedSwapPosts.Add(swap)
	}
}

func (scanner *ethSwapScanner) repostCachedSwaps() {
	for {
		scanner.cachedSwapPosts.Do(func(p interface{}) bool {
			return scanner.repostSwap(p.(*swapPost))
		})
		time.Sleep(30 * time.Second)
	}
}

func rpcPost(swap *swapPost) error {
	timeout := 300
	reqID := 666
	args := map[string]interface{}{
		"txid":   swap.txid,
		"pairid": swap.pairID,
	}
	var result interface{}
	return client.RPCPostWithTimeoutAndID(&result, timeout, reqID, swap.swapServer, swap.rpcMethod, args)
}

func (scanner *ethSwapScanner) repostSwap(swap *swapPost) bool {
	for i := 0; i < scanner.rpcRetryCount; i++ {
		err := rpcPost(swap)
		if tokens.ShouldRegisterSwapForError(err) {
			return true
		}
		if ctools.IsSwapAlreadyExistRegisterError(err) ||
			strings.Contains(err.Error(), swapExistKeywords) {
			return true
		}
		switch {
		case errors.Is(err, tokens.ErrTxNotFound):
		case strings.Contains(err.Error(), httpTimeoutKeywords):
		default:
			log.Warn("repost swap failed", "swap", swap, "err", err)
			return true
		}
		time.Sleep(scanner.rpcInterval)
	}
	return false
}

func (scanner *ethSwapScanner) getSwapoutFuncHashByTxType(txType string) []byte {
	switch strings.ToLower(txType) {
	case txSwapout:
		return addressSwapoutFuncHash
	case txSwapout2:
		return stringSwapoutFuncHash
	default:
		log.Errorf("unknown swapout tx type %v", txType)
		return nil
	}
}

func (scanner *ethSwapScanner) getLogTopicByTxType(txType string) common.Hash {
	switch strings.ToLower(txType) {
	case txSwapin:
		return transferLogTopic
	case txSwapout:
		return addressSwapoutLogTopic
	case txSwapout2:
		return stringSwapoutLogTopic
	default:
		log.Errorf("unknown tx type %v", txType)
		return common.Hash{}
	}
}

func (scanner *ethSwapScanner) verifyErc20SwapinTx(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (err error) {
	if receipt == nil {
		err = scanner.parseErc20SwapinTxInput(tx.Data(), tokenCfg.DepositAddress)
	} else {
		err = scanner.parseErc20SwapinTxLogs(receipt.Logs, tokenCfg.TokenAddress, tokenCfg.DepositAddress, tokenCfg.TxType)
	}
	return err
}

func (scanner *ethSwapScanner) verifySwapoutTx(tx *types.Transaction, receipt *types.Receipt, tokenCfg *params.TokenConfig) (err error) {
	if receipt == nil {
		err = scanner.parseSwapoutTxInput(tx.Data(), tokenCfg.TxType)
	} else {
		err = scanner.parseSwapoutTxLogs(receipt.Logs, tokenCfg.TokenAddress, tokenCfg.TxType)
	}
	return err
}

func (scanner *ethSwapScanner) parseErc20SwapinTxInput(input []byte, depositAddress string) error {
	if len(input) < 4 {
		return tokens.ErrTxWithWrongInput
	}
	var receiver string
	funcHash := input[:4]
	switch {
	case bytes.Equal(funcHash, transferFuncHash):
		receiver = common.BytesToAddress(common.GetData(input, 4, 32)).Hex()
	case bytes.Equal(funcHash, transferFromFuncHash):
		receiver = common.BytesToAddress(common.GetData(input, 36, 32)).Hex()
	default:
		return tokens.ErrTxFuncHashMismatch
	}
	if !strings.EqualFold(receiver, depositAddress) {
		return tokens.ErrTxWithWrongReceiver
	}
	return nil
}

func (scanner *ethSwapScanner) parseErc20SwapinTxLogs(logs []*types.Log, targetContract, depositAddress, txType string) (err error) {
	for _, log := range logs {
		if log.Removed {
			continue
		}
		if !strings.EqualFold(log.Address.Hex(), targetContract) {
			continue
		}
		if len(log.Topics) != 3 || log.Data == nil {
			continue
		}
		if log.Topics[0] == scanner.getLogTopicByTxType(txType) {
			receiver := common.BytesToAddress(log.Topics[2][:]).Hex()
			if strings.EqualFold(receiver, depositAddress) {
				return nil
			}
			return tokens.ErrTxWithWrongReceiver
		}
	}
	return tokens.ErrDepositLogNotFound
}

func (scanner *ethSwapScanner) parseSwapoutTxInput(input []byte, txType string) error {
	if len(input) < 4 {
		return tokens.ErrTxWithWrongInput
	}
	funcHash := input[:4]
	if bytes.Equal(funcHash, scanner.getSwapoutFuncHashByTxType(txType)) {
		return nil
	}
	return tokens.ErrTxFuncHashMismatch
}

func (scanner *ethSwapScanner) parseSwapoutTxLogs(logs []*types.Log, targetContract, txType string) (err error) {
	for _, log := range logs {
		if log.Removed {
			continue
		}
		if !strings.EqualFold(log.Address.Hex(), targetContract) {
			continue
		}
		if len(log.Topics) != 3 || log.Data == nil {
			continue
		}
		if log.Topics[0] == scanner.getLogTopicByTxType(txType) {
			return nil
		}
	}
	return tokens.ErrSwapoutLogNotFound
}

type cachedSacnnedBlocks struct {
	capacity  int
	nextIndex int
	hashes    []string
}

var cachedBlocks = &cachedSacnnedBlocks{
	capacity:  100,
	nextIndex: 0,
	hashes:    make([]string, 100),
}

func (cache *cachedSacnnedBlocks) addBlock(blockHash string) {
	cache.hashes[cache.nextIndex] = blockHash
	cache.nextIndex = (cache.nextIndex + 1) % cache.capacity
}

func (cache *cachedSacnnedBlocks) isScanned(blockHash string) bool {
	for _, b := range cache.hashes {
		if b == blockHash {
			return true
		}
	}
	return false
}
