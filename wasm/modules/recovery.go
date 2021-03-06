package modules

import (
	"fmt"
	"sync"
	"syscall/js"

	"github.com/siacentral/sia-lite-wallet-web/wasm/wallet"
)

type (
	recoveryWork struct {
		Round, Start, End uint64
	}

	recoveredAddress struct {
		Address          string                  `json:"address"`
		UsageType        string                  `json:"usage_type"`
		Index            uint64                  `json:"index"`
		UnlockConditions wallet.UnlockConditions `json:"unlock_conditions"`
	}

	recoveryResults struct {
		Round, LastUsedIndex, Start, End uint64
		LastUsedType                     string
		Addresses                        []recoveredAddress
		Error                            error
	}
)

func consecutiveEmptyRounds(rounds []uint64) uint64 {
	var lastRound uint64
	roundMap := make(map[uint64]bool)

	for _, r := range rounds {
		roundMap[r] = true

		if lastRound < r {
			lastRound = r
		}
	}

	i := lastRound

	for {
		if exists := roundMap[i]; !exists {
			break
		}

		i--
	}

	return lastRound - i
}

func generateAddress(w *wallet.SeedWallet, i uint64) recoveredAddress {
	key := w.GetAddress(i)
	addr := recoveredAddress{
		Address:          key.UnlockConditions.UnlockHash().String(),
		Index:            i,
		UnlockConditions: mapUnlockConditions(key.UnlockConditions),
	}

	return addr
}

func recoveryWorker(w *wallet.SeedWallet, currency string, work <-chan recoveryWork, results chan<- recoveryResults) {
	for r := range work {
		var addresses []string
		recovered := recoveryResults{
			Round: r.Round,
			Start: r.Start,
			End:   r.End,
		}

		addressMap := make(map[string]recoveredAddress)

		for i := r.Start; i < r.End; i++ {
			addr := generateAddress(w, i)
			addressMap[addr.Address] = addr
			addresses = append(addresses, addr.Address)
		}

		apiclient := siacentralAPIClient(currency)
		used, err := apiclient.FindUsedAddresses(addresses)

		if err != nil {
			results <- recoveryResults{
				Error: fmt.Errorf("unable to get used addresses: %w", err),
			}
			return
		}

		for _, usage := range used {
			addr, exists := addressMap[usage.Address]
			if !exists {
				continue
			}

			addr.UsageType = usage.UsageType
			recovered.Addresses = append(recovered.Addresses, addr)

			if recovered.LastUsedIndex < addr.Index {
				recovered.LastUsedIndex = addr.Index
				recovered.LastUsedType = addr.UsageType
			}
		}

		results <- recovered
	}
}

// RecoverAddresses scans for addresses on the blockchain addressCount at a time up to a maximum of 100,000,000
//addresses. Considers all addresses found if the scan goes more than minRounds * addressCount
//addresses without seeing any used. It's possible the ranges will need to be tweaked for older or
//larger wallets
func RecoverAddresses(seed, currency string, startIndex, maxEmptyRounds, addressCount, lastKnownIndex uint64, callback js.Value) {
	var wg sync.WaitGroup

	w, err := recoverWallet(seed, currency)

	if err != nil {
		callback.Invoke(fmt.Errorf("unable to recover wallet: %w", err).Error(), js.Null())
		return
	}

	work := make(chan recoveryWork, workers)
	results := make(chan recoveryResults)
	done := make(chan bool)

	wg.Add(workers)

	for i := 0; i < workers; i++ {
		go func() {
			recoveryWorker(w, currency, work, results)
			wg.Done()
		}()
	}

	go func() {
		// wait for all workers to drain the work channel, then stop
		wg.Wait()
		close(results)
	}()

	go func() {
		var round uint64

		for i := startIndex; ; i += addressCount {
			select {
			case <-done:
				close(work)
				return
			default:
			}

			work <- recoveryWork{
				Start: i,
				End:   i + addressCount,
				Round: round,
			}

			round++
		}
	}()

	var lastIndex, usedTotal uint64
	var lastUsageType string
	var empty []uint64

	for res := range results {
		if res.Error != nil {
			//close the done channel to signal completion if it isn't already closed
			select {
			case <-done:
				break
			default:
				close(done)
			}
			continue
		}

		if len(res.Addresses) == 0 && res.End >= lastKnownIndex {
			empty = append(empty, res.Round)

			if consecutive := consecutiveEmptyRounds(empty); consecutive >= maxEmptyRounds {
				//close the done channel to signal completion if it isn't already closed
				select {
				case <-done:
					break
				default:
					close(done)
				}
			}
		}

		usedTotal += uint64(len(res.Addresses))

		if res.LastUsedIndex > lastIndex {
			lastIndex = res.LastUsedIndex
			lastUsageType = res.LastUsedType
		}

		data, err := interfaceToJSON(map[string]interface{}{
			"found":     len(res.Addresses),
			"addresses": res.Addresses,
			"index":     lastIndex,
		})

		if err != nil {
			callback.Invoke(err.Error(), js.Null())
			return
		}

		callback.Invoke("progress", data)
	}

	var additional []recoveredAddress

	if lastUsageType == "sent" {
		lastIndex++

		additional = append(additional, generateAddress(w, lastIndex))
	}

	data, err := interfaceToJSON(map[string]interface{}{
		"addresses": additional,
		"index":     lastIndex,
	})

	if err != nil {
		callback.Invoke(err.Error(), js.Null())
		return
	}

	callback.Invoke(js.Null(), data)
}
