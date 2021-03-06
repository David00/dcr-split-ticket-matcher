package buyer

import (
	"bufio"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	unsafe_rand "math/rand"
	"os"
	"path/filepath"
	"time"

	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/dcrutil"
	"github.com/decred/dcrd/wire"
	pbm "github.com/matheusd/dcr-split-ticket-matcher/pkg/api/matcherrpc"
	"github.com/matheusd/dcr-split-ticket-matcher/pkg/buyer/internal/net"
	"github.com/matheusd/dcr-split-ticket-matcher/pkg/matcher"
	"github.com/matheusd/dcr-split-ticket-matcher/pkg/splitticket"
	"github.com/pkg/errors"
)

type buyerSessionParticipant struct {
	secretHash   splitticket.SecretNumberHash
	secretNb     splitticket.SecretNumber
	votePkScript []byte
	poolPkScript []byte
	amount       dcrutil.Amount
	ticket       *wire.MsgTx
	revocation   *wire.MsgTx
	voteAddress  dcrutil.Address
}

// Session is the structure that stores data for a single split ticket
// session in progress.
type Session struct {
	ID           matcher.ParticipantID
	Amount       dcrutil.Amount
	Fee          dcrutil.Amount
	PoolFee      dcrutil.Amount
	TicketPrice  dcrutil.Amount
	sessionToken []byte

	mainchainHash   *chainhash.Hash
	mainchainHeight uint32
	nbParticipants  uint32
	secretNb        splitticket.SecretNumber
	secretNbHash    splitticket.SecretNumberHash

	voteAddress         dcrutil.Address
	poolAddress         dcrutil.Address
	splitOutputAddress  dcrutil.Address
	ticketOutputAddress dcrutil.Address
	splitChange         *wire.TxOut
	splitInputs         []*wire.TxIn
	participants        []buyerSessionParticipant
	splitTxUtxoMap      splitticket.UtxoMap
	myIndex             uint32

	ticketTemplate *wire.MsgTx
	splitTx        *wire.MsgTx

	ticketsScriptSig    [][]byte // one for each participant
	revocationScriptSig []byte

	selectedTicket     *wire.MsgTx
	fundedSplitTx      *wire.MsgTx
	selectedRevocation *wire.MsgTx
	voterIndex         int
	selectedCoin       dcrutil.Amount
}

func (session *Session) secretHashes() []splitticket.SecretNumberHash {
	res := make([]splitticket.SecretNumberHash, len(session.participants))
	for i, p := range session.participants {
		res[i] = p.secretHash
	}
	return res
}

func (session *Session) secretNumbers() []splitticket.SecretNumber {
	res := make([]splitticket.SecretNumber, len(session.participants))
	for i, p := range session.participants {
		res[i] = p.secretNb
	}
	return res
}

func (session *Session) amounts() []dcrutil.Amount {
	res := make([]dcrutil.Amount, len(session.participants))
	for i, p := range session.participants {
		res[i] = p.amount
	}
	return res
}

func (session *Session) voteScripts() [][]byte {
	res := make([][]byte, len(session.participants))
	for i, p := range session.participants {
		res[i] = p.votePkScript
	}
	return res
}

func (session *Session) voteAddresses() []dcrutil.Address {
	res := make([]dcrutil.Address, len(session.participants))
	for i, p := range session.participants {
		res[i] = p.voteAddress
	}
	return res
}

func (session *Session) splitInputOutpoints() []wire.OutPoint {
	res := make([]wire.OutPoint, len(session.splitTx.TxIn))
	for i, in := range session.splitTx.TxIn {
		res[i] = in.PreviousOutPoint
	}
	return res
}

func (session *Session) myTotalAmountIn() dcrutil.Amount {
	total := dcrutil.Amount(0)
	for _, in := range session.splitInputs {
		entry, has := session.splitTxUtxoMap[in.PreviousOutPoint]
		if has {
			total += entry.Value
		}
	}
	return total
}

// Reporter is an interface that must be implemented to report status of a buyer
// session during its progress.
type Reporter interface {
	reportStage(context.Context, Stage, *Session, *Config)
	reportMatcherStatus(*pbm.StatusResponse)
	reportSavedSession(string)
	reportSrvRecordFound(record string)
	reportSrvLookupError(err error)
	reportSplitPublished()
	reportRightTicketPublished()
	reportWrongTicketPublished(ticket *chainhash.Hash, session *Session)
	reportBuyingError(err error)
}

type sessionWaiterResponse struct {
	mc      *matcherClient
	wc      *walletClient
	session *Session
	err     error
}

type unreportableError struct {
	e error
}

func (e unreportableError) unreportable() bool { return true }
func (e unreportableError) Error() string      { return e.e.Error() }

// BuySplitTicket performs the whole split ticket purchase process, given the
// config provided. The context may be canceled at any time to abort the session.
func BuySplitTicket(ctx context.Context, cfg *Config) error {
	rep := reporterFromContext(ctx)
	rep.reportStage(ctx, StageStarting, nil, cfg)

	err := buySplitTicket(ctx, cfg)
	if err != nil {
		rep.reportBuyingError(err)
	}

	return err
}

// buySplitTicket is the unexported version that performs the whole ticket buyer
// process.
func buySplitTicket(ctx context.Context, cfg *Config) error {

	if cfg.WalletHost == "127.0.0.1:0" {
		hosts, err := net.FindListeningWallets(cfg.WalletCertFile, cfg.ChainParams)
		if err != nil {
			return errors.Wrapf(err, "error finding running wallet")
		}

		if len(hosts) != 1 {
			return errors.Errorf("found different number of running wallets "+
				"(%d) than expected", len(hosts))
		}

		cfg.WalletHost = hosts[0]
	}

	resp := waitForSession(ctx, cfg)
	if resp.err != nil {
		return errors.Wrap(resp.err, "error waiting for session")
	}

	defer func() {
		resp.mc.close()
		resp.wc.close()
	}()

	ctxBuy, cancelBuy := context.WithTimeout(ctx, time.Second*time.Duration(cfg.MaxTime))
	reschan2 := make(chan error)
	go func() { reschan2 <- buySplitTicketInSession(ctxBuy, cfg, resp.mc, resp.wc, resp.session) }()

	select {
	case <-ctx.Done():
		<-reschan2 // Wait for f to return.
		cancelBuy()
		return ctx.Err()
	case err := <-reschan2:
		if err != nil {
			if _, unreportable := err.(unreportableError); !unreportable && !cfg.SkipReportErrorsToSvc {
				resp.mc.sendErrorReport(resp.session.ID, err)
			}
		}
		cancelBuy()
		return err
	}

}

func waitForSession(mainCtx context.Context, cfg *Config) sessionWaiterResponse {
	var err error

	rep := reporterFromContext(mainCtx)

	setupCtx, setupCancel := context.WithTimeout(mainCtx, time.Second*60)

	wcc := cfg.WalletConn
	if wcc == nil {
		rep.reportStage(setupCtx, StageConnectingToWallet, nil, cfg)
		wcc, err = connectToWallet(cfg.WalletHost, cfg.WalletCertFile)
		if err != nil {
			setupCancel()
			return sessionWaiterResponse{nil, nil, nil, errors.Wrap(err,
				"error trying to connect to wallet")}
		}
	}
	wc := &walletClient{
		wsvc:        wcc,
		chainParams: cfg.ChainParams,
	}

	err = wc.checkNetwork(setupCtx)
	if err != nil {
		setupCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrap(err,
			"error checking for wallet network")}
	}

	err = wc.testVoteAddress(setupCtx, cfg)
	if err != nil {
		setupCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrap(err,
			"error testing buyer vote address")}
	}

	err = wc.testPassphrase(setupCtx, cfg)
	if err != nil {
		setupCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrap(err,
			"error testing wallet passphrase")}
	}

	err = wc.testFunds(setupCtx, cfg)
	if err != nil {
		setupCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrap(err,
			"error testing wallet funds")}
	}

	var dcrd *decredNetwork

	mcc := cfg.MatcherConn
	if mcc == nil {
		var utxoProvider utxoMapProvider
		if !cfg.UtxosFromDcrdata {
			dcrd, err = connectToDecredNode(cfg.networkCfg())
			if err != nil {
				setupCancel()
				return sessionWaiterResponse{nil, nil, nil, errors.Wrap(err,
					"error connecting to dcrd")}
			}
			rep.reportStage(setupCtx, StageConnectingToDcrd, nil, cfg)
			utxoProvider = dcrd.fetchSplitUtxos
		} else {
			err = isDcrdataOnline(cfg.DcrdataURL, cfg.ChainParams)
			if err != nil {
				setupCancel()
				return sessionWaiterResponse{nil, nil, nil, errors.Wrap(err,
					"error checking if dcrdata is online")}
			}

			rep.reportStage(setupCtx, StageConnectingToDcrdata, nil, cfg)
			utxoProvider = utxoProviderForDcrdataURL(cfg.DcrdataURL)
		}

		rep.reportStage(setupCtx, StageConnectingToMatcher, nil, cfg)
		mcc, err = connectToMatcherService(setupCtx, cfg.MatcherHost, cfg.MatcherCertFile,
			utxoProvider)
		if err != nil {
			setupCancel()
			return sessionWaiterResponse{nil, nil, nil, errors.Wrapf(err,
				"error connecting to matcher")}
		}
	}
	mc := &matcherClient{mcc}

	status, err := mc.status(setupCtx)
	if err != nil {
		setupCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrapf(err,
			"error getting status from matcher")}
	}
	rep.reportMatcherStatus(status)

	maxAmount, err := dcrutil.NewAmount(cfg.MaxAmount)
	if err != nil {
		setupCancel()
		return sessionWaiterResponse{nil, nil, nil, err}
	}

	setupCancel()

	rep.reportStage(mainCtx, StageFindingMatches, nil, cfg)

	maxWaitTime := time.Duration(cfg.MaxWaitTime)
	if maxWaitTime <= 0 {
		maxWaitTime = 60 * 60 * 24 * 365 * 10 // 10 years is plenty :)
	}
	waitCtx, waitCancel := context.WithTimeout(mainCtx, time.Second*maxWaitTime)

	walletErrChan := make(chan error)
	go func() {
		err := wc.checkWalletWaitingForSession(waitCtx)
		if err != nil {
			walletErrChan <- err
		}
	}()

	dcrdErrChan := make(chan error)
	if dcrd != nil {
		go func() {
			err := dcrd.checkDcrdWaitingForSession(waitCtx)
			if err != nil {
				dcrdErrChan <- err
			}
		}()
	}

	checkSyncChan := make(chan error)
	go func() {
		err := checkMatcherWalletBlockchainSync(waitCtx, mc, wc)
		if err != nil {
			checkSyncChan <- err
		}
	}()

	sessionChan := make(chan *Session)
	participateErrChan := make(chan error)

	go func() {
		session, err := mc.participate(waitCtx, maxAmount, cfg.SessionName, cfg.VoteAddress,
			cfg.PoolAddress, cfg.PoolFeeRate, cfg.ChainParams)
		if err != nil {
			participateErrChan <- err
		} else {
			sessionChan <- session
		}
	}()

	select {
	case <-waitCtx.Done():
		waitCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrap(waitCtx.Err(),
			"timeout while waiting for session in matcher")}
	case walletErr := <-walletErrChan:
		waitCancel()
		return sessionWaiterResponse{nil, nil, nil, walletErr}
	case dcrdErr := <-dcrdErrChan:
		waitCancel()
		return sessionWaiterResponse{nil, nil, nil, dcrdErr}
	case partErr := <-participateErrChan:
		waitCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrap(partErr,
			"error while waiting to participate in session")}
	case syncErr := <-checkSyncChan:
		waitCancel()
		return sessionWaiterResponse{nil, nil, nil, errors.Wrap(syncErr,
			"error while checking matcher and wallet sync to the network")}
	case session := <-sessionChan:
		waitCancel()
		rep.reportStage(mainCtx, StageMatchesFound, session, cfg)
		return sessionWaiterResponse{mc, wc, session, nil}
	}
}

// checkMatcherWalletBlockchainSync checks whether the given matcher and wallet
// clients are synced to the same height.
//
// This function will keep checking the sync every 5 minutes or until the
// context is cancelled, so it needs to be called on a goroutine.
//
// It returns nil if the context was canceled or an error if the matcher and
// wallet grow out of sync.
func checkMatcherWalletBlockchainSync(waitCtx context.Context, mc *matcherClient, wc *walletClient) error {
	ticker := time.NewTicker(time.Minute * 5)
	defer ticker.Stop()

	checkSync := func() error {
		mcStatus, err := mc.status(waitCtx)
		if err != nil {
			return errors.Wrap(err, "error checking status of matcher")
		}

		wcChainInfo, err := wc.currentChainInfo(waitCtx)
		if err != nil {
			return errors.Wrap(err, "error checking wallet chain info")
		}

		hashMatcher, err := chainhash.NewHash(mcStatus.MainchainHash)
		if err != nil {
			return errors.Wrap(err, "matcher sent an invalid hash")
		}

		if !hashMatcher.IsEqual(wcChainInfo.bestBlockHash) {
			return errors.Errorf("matcher mainchain hash (%s) different than "+
				"wallet mainchain hash (%s)", hashMatcher, wcChainInfo.bestBlockHash)
		}

		return nil
	}

	for {
		select {
		case <-waitCtx.Done():
			return nil
		case <-ticker.C:
			err := checkSync()
			if err == nil {
				// matcher and wallet are in sync
				continue
			}

			// matcher and wallet are out of sync. Let's wait a 10 +- 10 seconds
			// to let them catch up in case they were just momentarily out of
			// sync, then try again.
			sleeptime := time.Duration(10 + unsafe_rand.Intn(10))
			time.Sleep(time.Second * sleeptime)
			err = checkSync()
			if err != nil {
				// an actual error in sync. return.
				return err
			}

			// wallet and matcher are in sync again, so just continue checking.
		}
	}
}

func buySplitTicketInSession(ctx context.Context, cfg *Config, mc *matcherClient, wc *walletClient, session *Session) error {

	rep := reporterFromContext(ctx)
	var err error

	chainInfo, err := wc.currentChainInfo(ctx)
	if err != nil {
		return err
	}
	if !chainInfo.bestBlockHash.IsEqual(session.mainchainHash) {
		return errors.Errorf("mainchain tip of wallet (%s) not the same as "+
			"matcher (%s)", chainInfo.bestBlockHash, session.mainchainHash)
	}
	if chainInfo.bestBlockHeight != session.mainchainHeight {
		return errors.Errorf("mainchain height of wallet (%d) not the same as "+
			"matcher (%d)", chainInfo.bestBlockHeight, session.mainchainHeight)
	}
	if chainInfo.ticketPrice != session.TicketPrice {
		return errors.Errorf("ticket price of wallet (%s) not the same as "+
			"matcher (%s)", chainInfo.ticketPrice, session.TicketPrice)
	}

	rep.reportStage(ctx, StageGeneratingOutputs, session, cfg)
	err = wc.generateOutputs(ctx, session, cfg)
	if err != nil {
		return err
	}
	rep.reportStage(ctx, StageOutputsGenerated, session, cfg)

	rep.reportStage(ctx, StageGeneratingTicket, session, cfg)
	err = mc.generateTicket(ctx, session, cfg)
	if err != nil {
		return unreportableError{err}
	}
	rep.reportStage(ctx, StageTicketGenerated, session, cfg)

	rep.reportStage(ctx, StageSigningTicket, session, cfg)
	err = wc.signTransactions(ctx, session, cfg)
	if err != nil {
		return err
	}
	rep.reportStage(ctx, StageTicketSigned, session, cfg)

	rep.reportStage(ctx, StageFundingTicket, session, cfg)
	err = mc.fundTicket(ctx, session, cfg)
	if err != nil {
		return unreportableError{err}
	}
	rep.reportStage(ctx, StageTicketFunded, session, cfg)

	err = wc.monitorSession(ctx, session)
	if err != nil {
		return errors.Wrapf(err, "error when trying to start monitoring for "+
			"session txs")
	}

	rep.reportStage(ctx, StageFundingSplitTx, session, cfg)
	err = mc.fundSplitTx(ctx, session, cfg)
	if err != nil {
		return unreportableError{err}
	}
	rep.reportStage(ctx, StageSplitTxFunded, session, cfg)

	err = saveSession(ctx, session, cfg)
	if err != nil {
		return errors.Wrapf(err, "error saving session")
	}

	if cfg.SkipWaitPublishedTxs {
		rep.reportStage(ctx, StageSkippedWaiting, session, cfg)
		rep.reportStage(ctx, StageSessionEndedSuccessfully, session, cfg)
		return nil
	}

	err = waitForPublishedTxs(ctx, session, cfg, wc)
	if err != nil {
		return unreportableError{errors.Wrapf(err, "error waiting for txs to be published")}
	}

	rep.reportStage(ctx, StageSessionEndedSuccessfully, session, cfg)
	return nil
}

func waitForPublishedTxs(ctx context.Context, session *Session,
	cfg *Config, wc *walletClient) error {

	var notifiedSplit, notifiedTicket bool
	rep := reporterFromContext(ctx)

	expectedTicketHash := session.selectedTicket.TxHash()
	correctTicket := false

	for !notifiedSplit || !notifiedTicket {
		select {
		case <-ctx.Done():
			ctxErr := ctx.Err()
			if ctxErr != nil {
				return errors.Wrapf(ctxErr, "context error while waiting for"+
					"published txs")
			}
			return errors.Errorf("context done while waiting for published" +
				"txs")
		default:
			if !notifiedSplit && wc.wsvc.PublishedSplitTx() {
				rep.reportSplitPublished()
				notifiedSplit = true
			}

			if !notifiedTicket && wc.wsvc.PublishedTicketTx() != nil {
				publishedHash := wc.wsvc.PublishedTicketTx()
				if expectedTicketHash.IsEqual(publishedHash) {
					rep.reportRightTicketPublished()
					correctTicket = true
				} else {
					rep.reportWrongTicketPublished(publishedHash, session)
				}
				notifiedTicket = true
			}

			time.Sleep(time.Millisecond * 250)
		}
	}

	if !correctTicket {
		return errors.Errorf("wrong ticket published to the network")
	}

	return nil
}

// SessionWriter is an interface defining the methods needed for writing
// information of a successful ticket session to disk.
type SessionWriter interface {
	io.Writer
	StartWritingSession(ticketHash string)
	SessionWritingFinished()
}

func saveSession(ctx context.Context, session *Session, cfg *Config) error {

	rep := reporterFromContext(ctx)

	ticketHashHex := session.selectedTicket.TxHash().String()
	ticketBytes, err := session.selectedTicket.Bytes()
	if err != nil {
		return err
	}

	var out func(format string, args ...interface{})
	var w *bufio.Writer
	var f *os.File
	var fname string
	var hexWriter io.Writer

	if cfg.SaveSessionWriter == nil {
		// Save directly to a file
		sessionDir := filepath.Join(cfg.DataDir, "sessions")
		_, err = os.Stat(sessionDir)

		if os.IsNotExist(err) {
			err = os.MkdirAll(sessionDir, 0700)
			if err != nil {
				return err
			}
		} else if err != nil {
			return err
		}

		fname = filepath.Join(sessionDir, ticketHashHex)

		fflags := os.O_TRUNC | os.O_CREATE | os.O_WRONLY
		f, err = os.OpenFile(fname, fflags, 0600)
		if err != nil {
			return err
		}
		w = bufio.NewWriter(f)

		defer func() {
			w.Flush()
			f.Sync()
			f.Close()
		}()

		out = func(format string, args ...interface{}) {
			w.WriteString(fmt.Sprintf(format, args...))
		}
		hexWriter = hex.NewEncoder(w)
	} else {
		// save using the writer
		cfg.SaveSessionWriter.StartWritingSession(ticketHashHex)
		out = func(format string, args ...interface{}) {
			cfg.SaveSessionWriter.Write([]byte(fmt.Sprintf(format, args...)))
		}
		hexWriter = hex.NewEncoder(cfg.SaveSessionWriter)
	}

	splitHash := session.fundedSplitTx.TxHash()
	splitBytes, err := session.fundedSplitTx.Bytes()
	if err != nil {
		return err
	}

	revocationHash := session.selectedRevocation.TxHash()
	revocationBytes, err := session.selectedRevocation.Bytes()
	if err != nil {
		return err
	}

	totalPoolFee := dcrutil.Amount(session.fundedSplitTx.TxOut[1].Value)
	contribPerc := float64(session.Amount+session.PoolFee) / float64(session.TicketPrice) * 100

	out("====== General Info ======\n")

	out("Session ID = %s\n", session.ID)
	out("Ending Time = %s\n", time.Now().String())
	out("Mainchain Hash = %s\n", session.mainchainHash.String())
	out("Mainchain Height = %d\n", session.mainchainHeight)
	out("Ticket Price = %s\n", session.TicketPrice)
	out("Number of Participants = %d\n", session.nbParticipants)
	out("My Index = %d\n", session.myIndex)
	out("My Secret Number = %s\n", session.secretNb)
	out("My Secret Hash = %s\n", session.secretNbHash)
	out("Commitment Amount = %s (%.2f%%)\n", session.Amount, contribPerc)
	out("Ticket Fee = %s (total = %s)\n", session.Fee, session.Fee*dcrutil.Amount(session.nbParticipants))
	out("Pool Fee = %s (total = %s)\n", session.PoolFee, totalPoolFee)
	out("Split Transaction hash = %s\n", splitHash.String())
	out("Final Ticket Hash = %s\n", ticketHashHex)
	out("Final Revocation Hash = %s\n", revocationHash.String())

	out("\n")
	out("====== Voter Selection ======\n")

	commitHash := splitticket.CalcLotteryCommitmentHash(
		session.secretHashes(), session.amounts(), session.voteAddresses(),
		session.mainchainHash)

	out("Participant Amounts = %v\n", session.amounts())
	out("Secret Hashes = %v\n", session.secretHashes())
	out("Voter Addresses = %v\n", encodedVoteAddresses(session.voteAddresses()))
	out("Voter Lottery Commitment Hash = %s\n", hex.EncodeToString(commitHash[:]))
	out("Secret Numbers = %v\n", session.secretNumbers())
	out("Selected Coin = %s\n", session.selectedCoin)
	out("Selected Voter Index = %d\n", session.voterIndex)

	out("\n")
	out("====== My Participation Info ======\n")
	out("Total input amount: %s\n", session.myTotalAmountIn())
	if session.splitChange != nil {
		out("Change amount: %s\n", dcrutil.Amount(session.splitChange.Value))
	} else {
		out("Change amount: [none]\n")
	}
	out("Commitment Address: %s\n", session.ticketOutputAddress.EncodeAddress())
	out("Split Output Address: %s\n", session.splitOutputAddress.EncodeAddress())
	out("Vote Address: %s\n", cfg.VoteAddress)
	out("Pool Fee Address: %s\n", cfg.PoolAddress)

	out("\n")
	out("====== Final Transactions ======\n")

	out("== Split Transaction ==\n")
	hexWriter.Write(splitBytes)
	out("\n\n")

	out("== Ticket ==\n")
	hexWriter.Write(ticketBytes)
	out("\n\n")

	out("== Revocation ==\n")
	hexWriter.Write(revocationBytes)
	out("\n\n")

	out("\n")
	out("====== My Split Inputs ======\n")
	for i, in := range session.splitInputs {
		out("Outpoint %d = %s\n", i, in.PreviousOutPoint)
	}

	out("\n")
	out("====== Participant Intermediate Information ======\n")
	for i, p := range session.participants {
		voteScript := hex.EncodeToString(p.votePkScript)
		poolScript := hex.EncodeToString(p.poolPkScript)

		partTicket, err := p.ticket.Bytes()
		if err != nil {
			return errors.Wrapf(err, "error encoding participant %d ticket", i)
		}

		partRevocation, err := p.revocation.Bytes()
		if err != nil {
			return errors.Wrapf(err, "error encoding participant %d revocation", i)
		}

		out("\n")
		out("== Participant %d ==\n", i)
		out("Amount = %s\n", p.amount)
		out("Secret Hash = %s\n", p.secretHash)
		out("Secret Number = %s\n", p.secretNb)
		out("Vote Address = %s\n", p.voteAddress.EncodeAddress())
		out("Vote PkScript = %s\n", voteScript)
		out("Pool PkScript = %s\n", poolScript)
		out("Ticket = %s\n", hex.EncodeToString(partTicket))
		out("Revocation = %s\n", hex.EncodeToString(partRevocation))
	}

	if cfg.SaveSessionWriter == nil {
		w.Flush()
		f.Sync()
		rep.reportSavedSession(fname)
	} else {
		cfg.SaveSessionWriter.SessionWritingFinished()
	}

	return nil
}
