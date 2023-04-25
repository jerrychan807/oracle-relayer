package relayer

import (
	"encoding/hex"
	"fmt"
	"time"

	"github.com/binance-chain/go-sdk/common/types"
	"github.com/binance-chain/go-sdk/types/msg"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/jinzhu/gorm"

	"github.com/binance-chain/oracle-relayer/common"
	"github.com/binance-chain/oracle-relayer/executor"
	"github.com/binance-chain/oracle-relayer/model"
	"github.com/binance-chain/oracle-relayer/util"
)

type Relayer struct {
	DB          *gorm.DB
	BBCExecutor executor.BbcExecutor
	Config      *util.Config
}

// NewRelayer returns the relayer instance
func NewRelayer(db *gorm.DB, bbcExecutor executor.BbcExecutor, cfg *util.Config) *Relayer {
	return &Relayer{
		DB:          db,
		BBCExecutor: bbcExecutor,
		Config:      cfg,
	}
}

// 启动中继器
// Main starts the routines of relayer
func (r *Relayer) Main() {
	go r.RelayPackages()

	go r.Alert()
}

// RelayPackages starts the main routine for processing the cross-chain packages
// 开始处理跨链数据包
func (r *Relayer) RelayPackages() {
	for {
		err := r.process(r.Config.ChainConfig.BSCChainId)
		if err != nil {
			time.Sleep(time.Duration(r.Config.ChainConfig.RelayInterval) * time.Millisecond)
		}
	}
}

// 发送跨链数据包到BC链
// 处理跨链数据包的具体函数,主要逻辑
// process relays the next batch of packages to Binance Chain
func (r *Relayer) process(chainId uint16) error {
	// 连接BC,获取当前序列号
	sequence, err := r.BBCExecutor.GetCurrentSequence(chainId)
	if err != nil {
		util.Logger.Errorf("get current sequence error: chainId=%d, err=%s",
			chainId, err.Error())
		return err
	}

	util.Logger.Infof("current sequence, chain_id=%d, seq=%d", chainId, sequence)

	// 从数据库中查出 所有需要声明的跨链包交易,这些交易都是bsc中继器调用Crosschain合约的交易
	// 这些交易都是 被确认过的交易
	claimLogs := make([]*model.CrossChainPackageLog, 0)
	err = r.DB.Where("oracle_sequence = ? and chain_id = ? and status = ?",
		sequence, chainId, model.PackageStatusConfirmed).Order("tx_index asc").Find(&claimLogs).Error
	if err != nil {
		util.Logger.Errorf("query claim log error: err=%s", err.Error())
		return err
	}
	fmt.Printf("[*] claimLogs: %v\n", claimLogs)
	fmt.Printf("[*] len claimLogs: %d\n", len(claimLogs))
	if len(claimLogs) == 0 {
		return fmt.Errorf("no packages found")
	}
	// 查询预言值
	prophecy, err := r.BBCExecutor.GetProphecy(chainId, sequence)
	fmt.Printf("[*] prophecy: %v\n", prophecy)
	if err != nil {
		util.Logger.Errorf("get prophecy error: err=%s", err.Error())
		return err
	}
	// 验证者地址
	validatorAddress := r.BBCExecutor.GetAddress()
	fmt.Printf("[*] validatorAddress: %s\n", validatorAddress)

	// 检查预言值是否已经被声明
	if prophecy != nil && prophecy.ValidatorClaims != nil && prophecy.ValidatorClaims[validatorAddress.String()] != "" {
		return fmt.Errorf("already claimed")
	}

	packages := make(msg.Packages, 0, len(claimLogs))
	// 跨链交易记录 构造成 数据包
	for _, claimLog := range claimLogs {
		fmt.Printf("[*] claimLog: %v\n", claimLog)
		payload, err := hex.DecodeString(claimLog.PayLoad)
		if err != nil {
			return fmt.Errorf("decode payload error, payload=%s", claimLog.PayLoad)
		}

		pack := msg.Package{
			ChannelId: types.IbcChannelID(claimLog.ChannelId),
			Sequence:  claimLog.PackageSequence,
			Payload:   payload,
		}
		packages = append(packages, pack)
	}

	encodedPackages, err := rlp.EncodeToBytes(packages)
	if err != nil {
		return fmt.Errorf("encode packages error, err=%s", err.Error())
	}

	util.Logger.Infof("claim, chain_id=%d, seq=%d, payload=%s",
		chainId, sequence, hex.EncodeToString(encodedPackages))
	// 处理声明
	//tmpChainId := uint16(715)
	txHash, err := r.BBCExecutor.Claim(chainId, uint64(sequence), encodedPackages)
	//txHash, err := r.BBCExecutor.Claim(tmpChainId, uint64(sequence), encodedPackages)
	if err != nil {
		util.Logger.Errorf("claim error: err=%s", err.Error())
		return err
	}
	// 更新数据库中的跨链包记录, 将状态改为已声明
	err = r.DB.Model(model.CrossChainPackageLog{}).Where("oracle_sequence = ? and chain_id = ?", sequence, chainId).Update(map[string]interface{}{
		"status":        model.PackageStatusClaimed,
		"claim_tx_hash": txHash,
		"update_time":   time.Now().Unix(),
	}).Error
	if err != nil {
		util.Logger.Errorf("update CrossChainPackageLog error, err=%s", err.Error())
	}
	return err
}

// Alert sends alert to tg group if there is any package delayed
func (r *Relayer) Alert() {
	for {
		time.Sleep(common.PackageDelayAlertInterval)

		sequence, err := r.BBCExecutor.GetCurrentSequence(r.Config.ChainConfig.BSCChainId)
		if err != nil {
			util.Logger.Errorf("get current sequence error: chainId=%d, err=%s",
				r.Config.ChainConfig.BSCChainId, err.Error())
			continue
		}

		claimLog := &model.CrossChainPackageLog{}

		err = r.DB.Where("chain_id = ? and status = ? and oracle_sequence >= ?",
			r.Config.ChainConfig.BSCChainId, model.PackageStatusConfirmed, sequence).Order("oracle_sequence asc").First(&claimLog).Error
		if err != nil && err != gorm.ErrRecordNotFound {
			util.Logger.Errorf("query claim log error: err=%s", err.Error())
			continue
		}

		if claimLog.Id == 0 {
			continue
		}

		if time.Now().Unix()-claimLog.UpdateTime > r.Config.AlertConfig.PackageDelayAlertThreshold {
			alertMsg := fmt.Sprintf("[%s] cross chain package was confirmed but not relayed, confiremd_time=%s, sequence=%d",
				r.Config.AlertConfig.Moniker, time.Unix(claimLog.UpdateTime, 0).String(), claimLog.OracleSequence)

			util.SendTelegramMessage(alertMsg)
			util.SendPagerDutyAlert(alertMsg, util.IncidentDedupKeyRelayError)
		}
	}
}
