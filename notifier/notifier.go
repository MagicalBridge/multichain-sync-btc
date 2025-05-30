package notifier

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"time"

	"github.com/ethereum/go-ethereum/log"

	"github.com/dapplink-labs/multichain-sync-btc/common/retry"
	"github.com/dapplink-labs/multichain-sync-btc/common/tasks"
	"github.com/dapplink-labs/multichain-sync-btc/database"
)

type Notifier struct {
	db             *database.DB
	businessIds    []string
	notifyClient   map[string]*NotifyClient
	resourceCtx    context.Context
	resourceCancel context.CancelFunc
	tasks          tasks.Group
	ticker         *time.Ticker

	shutdown context.CancelCauseFunc
	stopped  atomic.Bool
}

func NewNotifier(db *database.DB, shutdown context.CancelCauseFunc) (*Notifier, error) {
	businessList, err := db.Business.QueryBusinessList()
	if err != nil {
		log.Error("query business list fail", "err", err)
		return nil, err
	}

	var businessIds []string
	var notifyClient map[string]*NotifyClient
	for _, business := range businessList {
		log.Info("handle business id", "business", business.BusinessUid)
		businessIds = append(businessIds, business.BusinessUid)
		client, err := NewNotifierClient(business.NotifyUrl)
		if err != nil {
			log.Error("new notify client fail", "err", err)
			return nil, err
		}
		notifyClient[business.BusinessUid] = client
	}

	resCtx, resCancel := context.WithCancel(context.Background())
	return &Notifier{
		db:             db,
		notifyClient:   notifyClient,
		businessIds:    businessIds,
		resourceCtx:    resCtx,
		resourceCancel: resCancel,
		tasks: tasks.Group{HandleCrit: func(err error) {
			shutdown(fmt.Errorf("critical error in internals: %w", err))
		}},
		ticker: time.NewTicker(time.Second * 5),
	}, nil
}

func (nf *Notifier) Start(ctx context.Context) error {
	log.Info("start internals......")
	nf.tasks.Go(func() error {
		for {
			select {
			case <-nf.ticker.C:
				var txn []Transaction
				for _, businessId := range nf.businessIds {
					log.Info("txn and businessId", "txn", txn, "businessId", businessId)

					needNotifyDeposits, err := nf.db.Deposits.QueryNotifyDeposits(businessId)
					if err != nil {
						log.Error("Query notify deposits fail", "err", err)
						return err
					}

					needNotifyWithdraws, err := nf.db.Withdraws.QueryNotifyWithdraws(businessId)
					if err != nil {
						log.Error("Query notify deposits fail", "err", err)
						return err
					}

					needNotifyInternals, err := nf.db.Internals.QueryNotifyInternal(businessId)
					if err != nil {
						log.Error("Query notify deposits fail", "err", err)
						return err
					}
					notifyRequest, err := nf.BuildNotifyTransaction(needNotifyDeposits, needNotifyWithdraws, needNotifyInternals)

					// BeforeRequest
					err = nf.BeforeAfterNotify(businessId, true, false, needNotifyDeposits, needNotifyWithdraws, needNotifyInternals)
					if err != nil {
						log.Error("Before notify update status  fail", "err", err)
						return err
					}

					notify, err := nf.notifyClient[businessId].BusinessNotify(notifyRequest)
					if err != nil {
						log.Error("notify business platform fail", "err")
						return err
					}

					// AfterRequest
					err = nf.BeforeAfterNotify(businessId, true, notify, needNotifyDeposits, needNotifyWithdraws, needNotifyInternals)
					if err != nil {
						log.Error("After notify update status fail", "err", err)
						return err
					}

				}
			case <-nf.resourceCtx.Done():
				log.Info("stop internals in worker")
				return nil
			}
		}
	})
	return nil
}

func (nf *Notifier) Stop(ctx context.Context) error {
	var result error
	nf.resourceCancel()
	nf.ticker.Stop()
	if err := nf.tasks.Wait(); err != nil {
		result = errors.Join(result, fmt.Errorf("failed to await notify %w"), err)
		return result
	}
	log.Info("stop notify success")
	return nil
}

func (nf *Notifier) Stopped() bool {
	return nf.stopped.Load()
}

func (nf *Notifier) BeforeAfterNotify(businessId string, isBefore bool, notifySuccess bool, deposits []database.Deposits, withdraws []database.Withdraws, internals []database.Internals) error {
	var depositsNotifyStatus uint8
	var withdrawNotifyStatus database.TxStatus
	var internalNotifyStatus database.TxStatus
	if isBefore {
		depositsNotifyStatus = 2
		withdrawNotifyStatus = database.TxStatusNotified
		internalNotifyStatus = database.TxStatusNotified
	} else {
		if notifySuccess {
			depositsNotifyStatus = 3
			withdrawNotifyStatus = database.TxStatusSuccess
			internalNotifyStatus = database.TxStatusSuccess
		} else {
			depositsNotifyStatus = 1
			withdrawNotifyStatus = database.TxStatusWalletDone
			internalNotifyStatus = database.TxStatusWalletDone
		}
	}
	// 过滤状态为 0 的交易
	var updateStutusDepositTxn []database.Deposits
	for _, deposit := range deposits {
		if deposit.Status != 0 {
			updateStutusDepositTxn = append(updateStutusDepositTxn, deposit)
		}
	}
	retryStrategy := &retry.ExponentialStrategy{Min: 1000, Max: 20_000, MaxJitter: 250}
	if _, err := retry.Do[interface{}](nf.resourceCtx, 10, retryStrategy, func() (interface{}, error) {
		if err := nf.db.Transaction(func(tx *database.DB) error {
			if len(deposits) > 0 {
				if err := tx.Deposits.UpdateDepositsNotifyStatus(businessId, depositsNotifyStatus, updateStutusDepositTxn); err != nil {
					return err
				}
			}
			if len(withdraws) > 0 {
				if err := tx.Withdraws.UpdateWithdrawStatus(businessId, withdrawNotifyStatus, withdraws); err != nil {
					return err
				}
			}

			if len(internals) > 0 {
				if err := tx.Internals.UpdateInternalStatus(businessId, internalNotifyStatus, internals); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			log.Error("unable to persist batch", "err", err)
			return nil, err
		}
		return nil, nil
	}); err != nil {
		return err
	}
	return nil
}

func (nf *Notifier) BuildNotifyTransaction(deposits []database.Deposits, withdraws []database.Withdraws, internals []database.Internals) (*NotifyRequest, error) {
	var notifyTransactions []Transaction
	for _, deposit := range deposits {
		txItem := Transaction{
			BlockHash:   deposit.BlockHash,
			BlockNumber: deposit.BlockNumber.Uint64(),
			Hash:        deposit.Hash,
			FromAddress: deposit.FromAddress,
			ToAddress:   deposit.ToAddress,
			Value:       deposit.Amount,
			Fee:         deposit.Fee.String(),
			TxType:      "deposit",
			Confirms:    deposit.Confirms,
		}
		notifyTransactions = append(notifyTransactions, txItem)
	}

	for _, withdraw := range withdraws {
		txItem := Transaction{
			BlockHash:   withdraw.BlockHash,
			BlockNumber: withdraw.BlockNumber.Uint64(),
			Hash:        withdraw.Hash,
			FromAddress: withdraw.FromAddress,
			ToAddress:   withdraw.ToAddress,
			Value:       withdraw.Amount,
			// todo:
			//Fee:          withdraw.Fee,
			TxType:   "withdraw",
			Confirms: 0,
		}
		notifyTransactions = append(notifyTransactions, txItem)
	}

	for _, internal := range internals {
		txItem := Transaction{
			BlockHash:   internal.BlockHash,
			BlockNumber: internal.BlockNumber.Uint64(),
			Hash:        internal.Hash,
			FromAddress: internal.FromAddress,
			ToAddress:   internal.ToAddress,
			Value:       internal.Amount,
			// todo:
			//Fee:          withdraw.Fee,
			TxType:   "withdraw",
			Confirms: 0,
		}
		notifyTransactions = append(notifyTransactions, txItem)
	}
	notifyReq := &NotifyRequest{
		Txn: notifyTransactions,
	}
	return notifyReq, nil
}
