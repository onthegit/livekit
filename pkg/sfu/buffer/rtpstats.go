// Copyright 2023 LiveKit, Inc.
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

package buffer

import (
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/livekit/livekit-server/pkg/sfu/utils"
	"github.com/livekit/mediatransportutil"
	"github.com/livekit/protocol/livekit"
	"github.com/livekit/protocol/logger"
)

const (
	GapHistogramNumBins = 101
	NumSequenceNumbers  = 65536
	FirstSnapshotId     = 1
	SnInfoSize          = 8192
	SnInfoMask          = SnInfoSize - 1

	firstPacketTimeAdjustWindow    = 2 * time.Minute
	firstPacketTimeAdjustThreshold = 5 * time.Second
)

// -------------------------------------------------------

func RTPDriftToString(r *livekit.RTPDrift) string {
	if r == nil {
		return "-"
	}

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", r.StartTime.AsTime().Format(time.UnixDate), r.EndTime.AsTime().Format(time.UnixDate), r.Duration)
	str += fmt.Sprintf(", ts: %d|%d|%d", r.StartTimestamp, r.EndTimestamp, r.RtpClockTicks)
	str += fmt.Sprintf(", d: %d|%.2fms", r.DriftSamples, r.DriftMs)
	str += fmt.Sprintf(", cr: %.2f", r.ClockRate)
	return str
}

// -------------------------------------------------------

type RTPFlowState struct {
	IsNotHandled bool

	HasLoss            bool
	LossStartInclusive uint64
	LossEndExclusive   uint64

	IsDuplicate  bool
	IsOutOfOrder bool

	ExtSequenceNumber uint64
	ExtTimestamp      uint64
}

type IntervalStats struct {
	packets            uint64
	bytes              uint64
	headerBytes        uint64
	packetsPadding     uint64
	bytesPadding       uint64
	headerBytesPadding uint64
	packetsLost        uint64
	packetsOutOfOrder  uint64
	frames             uint32
}

type RTPDeltaInfo struct {
	StartTime            time.Time
	Duration             time.Duration
	Packets              uint32
	Bytes                uint64
	HeaderBytes          uint64
	PacketsDuplicate     uint32
	BytesDuplicate       uint64
	HeaderBytesDuplicate uint64
	PacketsPadding       uint32
	BytesPadding         uint64
	HeaderBytesPadding   uint64
	PacketsLost          uint32
	PacketsMissing       uint32
	PacketsOutOfOrder    uint32
	Frames               uint32
	RttMax               uint32
	JitterMax            float64
	Nacks                uint32
	Plis                 uint32
	Firs                 uint32
}

type Snapshot struct {
	startTime             time.Time
	extStartSN            uint64
	extStartSNOverridden  uint64
	packetsDuplicate      uint64
	bytesDuplicate        uint64
	headerBytesDuplicate  uint64
	packetsLostOverridden uint64
	nacks                 uint32
	plis                  uint32
	firs                  uint32
	maxRtt                uint32
	maxJitter             float64
	maxJitterOverridden   float64
}

type SnInfo struct {
	hdrSize       uint16
	pktSize       uint16
	isPaddingOnly bool
	marker        bool
	isOutOfOrder  bool
}

type RTCPSenderReportData struct {
	RTPTimestamp     uint32
	RTPTimestampExt  uint64
	NTPTimestamp     mediatransportutil.NtpTime
	PacketCount      uint32
	PacketCountExt   uint64
	PaddingOnlyDrops uint64
	At               time.Time
}

type RTPStatsParams struct {
	ClockRate              uint32
	IsReceiverReportDriven bool
	Logger                 logger.Logger
}

type RTPStats struct {
	params RTPStatsParams
	logger logger.Logger

	lock sync.RWMutex

	initialized                    bool
	resyncOnNextPacket             bool
	shouldDiscountPaddingOnlyDrops bool

	startTime time.Time
	endTime   time.Time

	sequenceNumber *utils.WrapAround[uint16, uint64]

	extHighestSNOverridden uint64
	lastRRTime             time.Time
	lastRR                 rtcp.ReceptionReport

	timestamp *utils.WrapAround[uint32, uint64]

	firstTime   time.Time
	highestTime time.Time

	lastTransit   uint32
	lastJitterRTP uint32

	bytes                uint64
	headerBytes          uint64
	bytesDuplicate       uint64
	headerBytesDuplicate uint64
	bytesPadding         uint64
	headerBytesPadding   uint64
	packetsDuplicate     uint64
	packetsPadding       uint64

	packetsOutOfOrder uint64

	packetsLost           uint64
	packetsLostOverridden uint64

	frames uint32

	jitter              float64
	maxJitter           float64
	jitterOverridden    float64
	maxJitterOverridden float64

	snInfos        [SnInfoSize]SnInfo
	snInfoWritePtr int

	gapHistogram [GapHistogramNumBins]uint32

	nacks        uint32
	nackAcks     uint32
	nackMisses   uint32
	nackRepeated uint32

	plis    uint32
	lastPli time.Time

	layerLockPlis    uint32
	lastLayerLockPli time.Time

	firs    uint32
	lastFir time.Time

	keyFrames    uint32
	lastKeyFrame time.Time

	rtt    uint32
	maxRtt uint32

	srFirst  *RTCPSenderReportData
	srNewest *RTCPSenderReportData

	nextSnapshotId uint32
	snapshots      map[uint32]*Snapshot
}

func NewRTPStats(params RTPStatsParams) *RTPStats {
	return &RTPStats{
		params:         params,
		logger:         params.Logger,
		sequenceNumber: utils.NewWrapAround[uint16, uint64](),
		timestamp:      utils.NewWrapAround[uint32, uint64](),
		nextSnapshotId: FirstSnapshotId,
		snapshots:      make(map[uint32]*Snapshot),
	}
}

func (r *RTPStats) Seed(from *RTPStats) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if from == nil || !from.initialized {
		return
	}

	r.initialized = from.initialized
	r.resyncOnNextPacket = from.resyncOnNextPacket
	r.shouldDiscountPaddingOnlyDrops = from.shouldDiscountPaddingOnlyDrops

	r.startTime = from.startTime
	// do not clone endTime as a non-zero endTime indicates an ended object

	r.sequenceNumber.Seed(from.sequenceNumber)

	r.extHighestSNOverridden = from.extHighestSNOverridden
	r.lastRRTime = from.lastRRTime
	r.lastRR = from.lastRR

	r.timestamp.Seed(from.timestamp)

	r.firstTime = from.firstTime
	r.highestTime = from.highestTime

	r.lastTransit = from.lastTransit
	r.lastJitterRTP = from.lastJitterRTP

	r.bytes = from.bytes
	r.headerBytes = from.headerBytes
	r.bytesDuplicate = from.bytesDuplicate
	r.headerBytesDuplicate = from.headerBytesDuplicate
	r.bytesPadding = from.bytesPadding
	r.headerBytesPadding = from.headerBytesPadding
	r.packetsDuplicate = from.packetsDuplicate
	r.packetsPadding = from.packetsPadding

	r.packetsOutOfOrder = from.packetsOutOfOrder

	r.packetsLost = from.packetsLost
	r.packetsLostOverridden = from.packetsLostOverridden

	r.frames = from.frames

	r.jitter = from.jitter
	r.maxJitter = from.maxJitter
	r.jitterOverridden = from.jitterOverridden
	r.maxJitterOverridden = from.maxJitterOverridden

	r.snInfos = from.snInfos
	r.snInfoWritePtr = from.snInfoWritePtr

	r.gapHistogram = from.gapHistogram

	r.nacks = from.nacks
	r.nackAcks = from.nackAcks
	r.nackMisses = from.nackMisses
	r.nackRepeated = from.nackRepeated

	r.plis = from.plis
	r.lastPli = from.lastPli

	r.layerLockPlis = from.layerLockPlis
	r.lastLayerLockPli = from.lastLayerLockPli

	r.firs = from.firs
	r.lastFir = from.lastFir

	r.keyFrames = from.keyFrames
	r.lastKeyFrame = from.lastKeyFrame

	r.rtt = from.rtt
	r.maxRtt = from.maxRtt

	if from.srFirst != nil {
		srFirst := *from.srFirst
		r.srFirst = &srFirst
	} else {
		r.srFirst = nil
	}
	if from.srNewest != nil {
		srNewest := *from.srNewest
		r.srNewest = &srNewest
	} else {
		r.srNewest = nil
	}

	r.nextSnapshotId = from.nextSnapshotId
	for id, ss := range from.snapshots {
		ssCopy := *ss
		r.snapshots[id] = &ssCopy
	}
}

func (r *RTPStats) SetLogger(logger logger.Logger) {
	r.logger = logger
}

func (r *RTPStats) Stop() {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.endTime = time.Now()
}

func (r *RTPStats) NewSnapshotId() uint32 {
	r.lock.Lock()
	defer r.lock.Unlock()

	id := r.nextSnapshotId
	if r.initialized {
		extStartSN := r.sequenceNumber.GetExtendedStart()
		r.snapshots[id] = &Snapshot{
			startTime:            time.Now(),
			extStartSN:           extStartSN,
			extStartSNOverridden: extStartSN,
		}
	}

	r.nextSnapshotId++

	return id
}

func (r *RTPStats) IsActive() bool {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.initialized && r.endTime.IsZero()
}

func (r *RTPStats) Update(rtph *rtp.Header, payloadSize int, paddingSize int, packetTime time.Time) (flowState RTPFlowState) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		flowState.IsNotHandled = true
		return
	}

	if r.resyncOnNextPacket {
		r.resyncOnNextPacket = false

		if r.initialized {
			extHighestSN := r.sequenceNumber.GetExtendedHighest()
			var newestPacketCount uint64
			var paddingOnlyDrops uint64
			var extExpectedHighestSN uint64
			var expectedHighestSN uint16
			var snCycles uint64

			extHighestTS := r.timestamp.GetExtendedHighest()
			var newestTS uint64
			var extExpectedHighestTS uint64
			var expectedHighestTS uint32
			var tsCycles uint64
			if r.srNewest != nil {
				newestPacketCount = r.srNewest.PacketCountExt
				paddingOnlyDrops = r.srNewest.PaddingOnlyDrops
				if newestPacketCount != 0 {
					extExpectedHighestSN = r.sequenceNumber.GetExtendedStart() + newestPacketCount
					if r.shouldDiscountPaddingOnlyDrops {
						extExpectedHighestSN -= paddingOnlyDrops
					}
					expectedHighestSN = uint16(extExpectedHighestSN & 0xFFFF)
					snCycles = extExpectedHighestSN & 0xFFFF_FFFF_FFFF_0000
					if rtph.SequenceNumber-expectedHighestSN < (1<<15) && rtph.SequenceNumber < expectedHighestSN {
						snCycles += (1 << 16)
					}
					if snCycles != 0 && expectedHighestSN-rtph.SequenceNumber < (1<<15) && expectedHighestSN < rtph.SequenceNumber {
						snCycles -= (1 << 16)
					}
				}

				newestTS = r.srNewest.RTPTimestampExt
				extExpectedHighestTS = newestTS
				expectedHighestTS = uint32(extExpectedHighestTS & 0xFFFF_FFFF)
				tsCycles = extExpectedHighestTS & 0xFFFF_FFFF_0000_0000
				if rtph.Timestamp-expectedHighestTS < (1<<31) && rtph.Timestamp < expectedHighestTS {
					tsCycles += (1 << 32)
				}
				if tsCycles != 0 && expectedHighestTS-rtph.Timestamp < (1<<31) && expectedHighestTS < rtph.Timestamp {
					tsCycles -= (1 << 32)
				}
			}
			r.sequenceNumber.ResetHighest(snCycles + uint64(rtph.SequenceNumber) - 1)
			r.timestamp.ResetHighest(tsCycles + uint64(rtph.Timestamp))
			r.highestTime = packetTime
			r.logger.Debugw(
				"resync",
				"newestPacketCount", newestPacketCount,
				"paddingOnlyDrops", paddingOnlyDrops,
				"extExpectedHighestSN", extExpectedHighestSN,
				"expectedHighestSN", expectedHighestSN,
				"snCycles", snCycles,
				"rtpSN", rtph.SequenceNumber,
				"beforeExtHighestSN", extHighestSN,
				"afterExtHighestSN", r.sequenceNumber.GetExtendedHighest(),
				"newestTS", newestTS,
				"extExpectedHighestTS", extExpectedHighestTS,
				"expectedHighestTS", expectedHighestTS,
				"tsCycles", tsCycles,
				"rtpTS", rtph.Timestamp,
				"beforeExtHighestTS", extHighestTS,
				"afterExtHighestTS", r.timestamp.GetExtendedHighest(),
			)
		}
	}

	var resSN utils.WrapAroundUpdateResult[uint64]
	var resTS utils.WrapAroundUpdateResult[uint64]
	if !r.initialized {
		if payloadSize == 0 {
			// do not start on a padding only packet
			flowState.IsNotHandled = true
			return
		}

		r.initialized = true

		r.startTime = time.Now()

		r.firstTime = packetTime
		r.highestTime = packetTime

		resSN = r.sequenceNumber.Update(rtph.SequenceNumber)
		resTS = r.timestamp.Update(rtph.Timestamp)

		// initialize snapshots if any
		for i := uint32(FirstSnapshotId); i < r.nextSnapshotId; i++ {
			extStartSN := r.sequenceNumber.GetExtendedStart()
			r.snapshots[i] = &Snapshot{
				startTime:            r.startTime,
				extStartSN:           extStartSN,
				extStartSNOverridden: extStartSN,
			}
		}

		r.logger.Debugw(
			"rtp stream start",
			"startTime", r.startTime.String(),
			"firstTime", r.firstTime.String(),
			"startSN", r.sequenceNumber.GetExtendedStart(),
			"startTS", r.timestamp.GetExtendedStart(),
		)
	} else {
		resSN = r.sequenceNumber.Update(rtph.SequenceNumber)
		resTS = r.timestamp.Update(rtph.Timestamp)
	}

	hdrSize := uint64(rtph.MarshalSize())
	pktSize := hdrSize + uint64(payloadSize+paddingSize)
	gapSN := int64(resSN.ExtendedVal - resSN.PreExtendedHighest)
	if gapSN <= 0 { // duplicate OR out-of-order
		if payloadSize == 0 {
			// do not start on a padding only packet
			if resTS.IsRestart {
				r.logger.Infow("rolling back timestamp restart", "tsBefore", r.timestamp.GetExtendedStart(), "tsAfter", resTS.PreExtendedStart)
				r.timestamp.RollbackRestart(resTS.PreExtendedStart)
			}
			if resSN.IsRestart {
				r.logger.Infow("rolling back sequence number restart", "snBefore", r.sequenceNumber.GetExtendedStart(), "snAfter", resSN.PreExtendedStart)
				r.sequenceNumber.RollbackRestart(resSN.PreExtendedStart)
				return
			}
		}

		if gapSN != 0 {
			r.packetsOutOfOrder++
		}

		if resSN.IsRestart {
			r.packetsLost += resSN.PreExtendedStart - resSN.ExtendedVal

			extStartSN := r.sequenceNumber.GetExtendedStart()
			for _, s := range r.snapshots {
				if s.extStartSN == resSN.PreExtendedStart {
					s.extStartSN = extStartSN
				}
			}

			r.logger.Infow(
				"adjusting start sequence number",
				"snBefore", resSN.PreExtendedStart,
				"snAfter", resSN.ExtendedVal,
			)
		}

		if resTS.IsRestart {
			r.logger.Infow(
				"adjusting start timestamp",
				"tsBefore", resTS.PreExtendedStart,
				"tsAfter", resTS.ExtendedVal,
			)
		}

		if !r.isSnInfoLost(resSN.ExtendedVal, resSN.PreExtendedHighest) {
			r.bytesDuplicate += pktSize
			r.headerBytesDuplicate += hdrSize
			r.packetsDuplicate++
			flowState.IsDuplicate = true
		} else {
			r.packetsLost--
			r.setSnInfo(resSN.ExtendedVal, resSN.PreExtendedHighest, uint16(pktSize), uint16(hdrSize), uint16(payloadSize), rtph.Marker, true)
		}

		flowState.IsOutOfOrder = true
		flowState.ExtSequenceNumber = resSN.ExtendedVal
		flowState.ExtTimestamp = resTS.ExtendedVal
	} else { // in-order
		// update gap histogram
		r.updateGapHistogram(int(gapSN))

		// update missing sequence numbers
		r.clearSnInfos(resSN.PreExtendedHighest+1, resSN.ExtendedVal)
		r.packetsLost += uint64(gapSN - 1)

		r.setSnInfo(resSN.ExtendedVal, resSN.PreExtendedHighest, uint16(pktSize), uint16(hdrSize), uint16(payloadSize), rtph.Marker, false)

		if rtph.Timestamp != uint32(resTS.PreExtendedHighest) {
			// update only on first packet as same timestamp could be in multiple packets.
			// NOTE: this may not be the first packet with this time stamp if there is packet loss.
			r.highestTime = packetTime
		}

		if gapSN > 1 {
			flowState.HasLoss = true
			flowState.LossStartInclusive = resSN.PreExtendedHighest + 1
			flowState.LossEndExclusive = resSN.ExtendedVal
		}
		flowState.ExtSequenceNumber = resSN.ExtendedVal
		flowState.ExtTimestamp = resTS.ExtendedVal
	}

	if !flowState.IsDuplicate {
		if payloadSize == 0 {
			r.packetsPadding++
			r.bytesPadding += pktSize
			r.headerBytesPadding += hdrSize
		} else {
			r.bytes += pktSize
			r.headerBytes += hdrSize

			if rtph.Marker {
				r.frames++
			}

			r.updateJitter(rtph, packetTime)
		}
	}
	return
}

func (r *RTPStats) ResyncOnNextPacket(shouldDiscountPaddingOnlyDrops bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.resyncOnNextPacket = true
	r.shouldDiscountPaddingOnlyDrops = shouldDiscountPaddingOnlyDrops
}

func (r *RTPStats) getPacketsExpected() uint64 {
	return r.sequenceNumber.GetExtendedHighest() - r.sequenceNumber.GetExtendedStart() + 1
}

func (r *RTPStats) GetTotalPacketsPrimary() uint64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.getTotalPacketsPrimary()
}

func (r *RTPStats) getTotalPacketsPrimary() uint64 {
	packetsExpected := r.getPacketsExpected()
	if r.packetsLost > packetsExpected {
		// should not happen
		return 0
	}

	packetsSeen := packetsExpected - r.packetsLost
	if r.packetsPadding > packetsSeen {
		return 0
	}

	return packetsSeen - r.packetsPadding
}

func (r *RTPStats) UpdateFromReceiverReport(rr rtcp.ReceptionReport) (rtt uint32, isRttChanged bool) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.initialized || !r.endTime.IsZero() || !r.params.IsReceiverReportDriven {
		// it is possible that the `LastSequenceNumber` in the receiver report is before the starting
		// sequence number when dummy packets are used to trigger Pion's OnTrack path.
		return
	}

	extHighestSNOverridden := r.extHighestSNOverridden&0xFFFF_FFFF_0000_0000 + uint64(rr.LastSequenceNumber)
	if !r.lastRRTime.IsZero() {
		if (rr.LastSequenceNumber-r.lastRR.LastSequenceNumber) < (1<<31) && rr.LastSequenceNumber < r.lastRR.LastSequenceNumber {
			extHighestSNOverridden += (1 << 32)
		}
	}
	if extHighestSNOverridden < r.sequenceNumber.GetExtendedStart() {
		// it is possible that the `LastSequenceNumber` in the receiver report is before the starting
		// sequence number when dummy packets are used to trigger Pion's OnTrack path.
		r.lastRRTime = time.Now()
		r.lastRR = rr
		return
	}

	var err error
	if r.srNewest != nil {
		rtt, err = mediatransportutil.GetRttMs(&rr, r.srNewest.NTPTimestamp, r.srNewest.At)
		if err == nil {
			isRttChanged = rtt != r.rtt
		} else {
			if !errors.Is(err, mediatransportutil.ErrRttNotLastSenderReport) && !errors.Is(err, mediatransportutil.ErrRttNoLastSenderReport) {
				r.logger.Warnw("error getting rtt", err)
			}
		}
	}

	if r.lastRRTime.IsZero() || r.extHighestSNOverridden <= extHighestSNOverridden {
		r.extHighestSNOverridden = extHighestSNOverridden

		packetsLostOverridden := r.packetsLostOverridden&0xFFFF_FFFF_0000_0000 + uint64(rr.TotalLost)
		if (rr.TotalLost-r.lastRR.TotalLost) < (1<<31) && rr.TotalLost < r.lastRR.TotalLost {
			packetsLostOverridden += (1 << 32)
		}
		r.packetsLostOverridden = packetsLostOverridden

		if isRttChanged {
			r.rtt = rtt
			if rtt > r.maxRtt {
				r.maxRtt = rtt
			}
		}

		r.jitterOverridden = float64(rr.Jitter)
		if r.jitterOverridden > r.maxJitterOverridden {
			r.maxJitterOverridden = r.jitterOverridden
		}

		// update snapshots
		for _, s := range r.snapshots {
			if isRttChanged && rtt > s.maxRtt {
				s.maxRtt = rtt
			}

			if r.jitterOverridden > s.maxJitterOverridden {
				s.maxJitterOverridden = r.jitterOverridden
			}
		}

		r.lastRRTime = time.Now()
		r.lastRR = rr
	} else {
		r.logger.Debugw(
			fmt.Sprintf("receiver report potentially out of order, highestSN: existing: %d, received: %d", r.extHighestSNOverridden, rr.LastSequenceNumber),
			"lastRRTime", r.lastRRTime,
			"lastRR", r.lastRR,
			"sinceLastRR", time.Since(r.lastRRTime),
			"receivedRR", rr,
		)
	}
	return
}

func (r *RTPStats) LastReceiverReport() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastRRTime
}

func (r *RTPStats) UpdateNack(nackCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nacks += nackCount
}

func (r *RTPStats) UpdateNackProcessed(nackAckCount uint32, nackMissCount uint32, nackRepeatedCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.nackAcks += nackAckCount
	r.nackMisses += nackMissCount
	r.nackRepeated += nackRepeatedCount
}

func (r *RTPStats) UpdatePliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
	r.updatePliTimeLocked()
}

func (r *RTPStats) UpdatePli(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliLocked(pliCount)
}

func (r *RTPStats) updatePliLocked(pliCount uint32) {
	r.plis += pliCount
}

func (r *RTPStats) UpdatePliTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.updatePliTimeLocked()
}

func (r *RTPStats) updatePliTimeLocked() {
	r.lastPli = time.Now()
}

func (r *RTPStats) LastPli() time.Time {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.lastPli
}

func (r *RTPStats) TimeSinceLastPli() int64 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return time.Now().UnixNano() - r.lastPli.UnixNano()
}

func (r *RTPStats) UpdateLayerLockPliAndTime(pliCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.layerLockPlis += pliCount
	r.lastLayerLockPli = time.Now()
}

func (r *RTPStats) UpdateFir(firCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.firs += firCount
}

func (r *RTPStats) UpdateFirTime() {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.lastFir = time.Now()
}

func (r *RTPStats) UpdateKeyFrame(kfCount uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.keyFrames += kfCount
	r.lastKeyFrame = time.Now()
}

func (r *RTPStats) UpdateRtt(rtt uint32) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.endTime.IsZero() {
		return
	}

	r.rtt = rtt
	if rtt > r.maxRtt {
		r.maxRtt = rtt
	}

	for _, s := range r.snapshots {
		if rtt > s.maxRtt {
			s.maxRtt = rtt
		}
	}
}

func (r *RTPStats) GetRtt() uint32 {
	r.lock.RLock()
	defer r.lock.RUnlock()

	return r.rtt
}

func (r *RTPStats) MaybeAdjustFirstPacketTime(srData *RTCPSenderReportData) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if srData != nil {
		r.maybeAdjustFirstPacketTime(srData.RTPTimestampExt)
	}
}

func (r *RTPStats) maybeAdjustFirstPacketTime(ets uint64) {
	if time.Since(r.startTime) > firstPacketTimeAdjustWindow {
		return
	}

	// for some time after the start, adjust time of first packet.
	// Helps improve accuracy of expected timestamp calculation.
	// Adjusting only one way, i. e. if the first sample experienced
	// abnormal delay (maybe due to pacing or maybe due to queuing
	// in some network element along the way), push back first time
	// to an earlier instance.
	samplesDiff := int64(ets - r.timestamp.GetExtendedStart())
	if samplesDiff < 0 {
		// out-of-order, skip
		return
	}
	samplesDuration := time.Duration(float64(samplesDiff) / float64(r.params.ClockRate) * float64(time.Second))
	now := time.Now()
	firstTime := now.Add(-samplesDuration)
	if firstTime.Before(r.firstTime) {
		r.logger.Debugw(
			"adjusting first packet time",
			"startTime", r.startTime.String(),
			"nowTime", now.String(),
			"before", r.firstTime.String(),
			"after", firstTime.String(),
			"adjustment", r.firstTime.Sub(firstTime),
			"extNowTS", ets,
			"extStartTS", r.timestamp.GetExtendedStart(),
		)
		if r.firstTime.Sub(firstTime) > firstPacketTimeAdjustThreshold {
			r.logger.Infow("first packet time adjustment too big, ignoring",
				"startTime", r.startTime.String(),
				"nowTime", now.String(),
				"before", r.firstTime.String(),
				"after", firstTime.String(),
				"adjustment", r.firstTime.Sub(firstTime),
				"extNowTS", ets,
				"extStartTS", r.timestamp.GetExtendedStart(),
			)
		} else {
			r.firstTime = firstTime
		}
	}
}

func (r *RTPStats) SetRtcpSenderReportData(srData *RTCPSenderReportData) {
	r.lock.Lock()
	defer r.lock.Unlock()

	if srData == nil || !r.initialized {
		return
	}

	// prevent against extreme case of anachronous sender reports
	if r.srNewest != nil && r.srNewest.NTPTimestamp > srData.NTPTimestamp {
		r.logger.Infow(
			"received anachronous sender report",
			"currentNTP", srData.NTPTimestamp.Time().String(),
			"currentRTP", srData.RTPTimestamp,
			"currentAt", srData.At.String(),
			"lastNTP", r.srNewest.NTPTimestamp.Time().String(),
			"lastRTP", r.srNewest.RTPTimestamp,
			"lastAt", r.srNewest.At.String(),
		)
		return
	}

	tsCycles := uint64(0)
	pcCycles := uint64(0)
	if r.srNewest != nil {
		tsCycles = r.srNewest.RTPTimestampExt & 0xFFFF_FFFF_0000_0000
		if (srData.RTPTimestamp-r.srNewest.RTPTimestamp) < (1<<31) && srData.RTPTimestamp < r.srNewest.RTPTimestamp {
			tsCycles += (1 << 32)
		}

		pcCycles = r.srNewest.PacketCountExt & 0xFFFF_FFFF_0000_0000
		if (srData.PacketCount-r.srNewest.PacketCount) < (1<<31) && srData.PacketCount < r.srNewest.PacketCount {
			pcCycles += (1 << 32)
		}
	}

	srDataCopy := *srData
	srDataCopy.RTPTimestampExt = uint64(srDataCopy.RTPTimestamp) + tsCycles
	srDataCopy.PacketCountExt = uint64(srDataCopy.PacketCount) + pcCycles

	r.maybeAdjustFirstPacketTime(srDataCopy.RTPTimestampExt)

	if r.srNewest != nil && srDataCopy.RTPTimestampExt < r.srNewest.RTPTimestampExt {
		// This can happen when a track is replaced with a null and then restored -
		// i. e. muting replacing with null and unmute restoring the original track.
		// Under such a condition reset the sender reports to start from this point.
		// Resetting will ensure sample rate calculations do not go haywire due to negative time.
		r.logger.Infow(
			"received sender report, out-of-order, resetting",
			"prevTSExt", r.srNewest.RTPTimestampExt,
			"prevRTP", r.srNewest.RTPTimestamp,
			"prevNTP", r.srNewest.NTPTimestamp.Time().String(),
			"prevAt", r.srNewest.At.String(),
			"currTSExt", srDataCopy.RTPTimestampExt,
			"currRTP", srDataCopy.RTPTimestamp,
			"currNTP", srDataCopy.NTPTimestamp.Time().String(),
			"currentAt", srDataCopy.At.String(),
		)
		r.srFirst = nil
	}

	r.srNewest = &srDataCopy
	if r.srFirst == nil {
		r.srFirst = &srDataCopy
	}
}

func (r *RTPStats) GetRtcpSenderReportData() (srFirst *RTCPSenderReportData, srNewest *RTCPSenderReportData) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.srFirst != nil {
		srFirstCopy := *r.srFirst
		srFirst = &srFirstCopy
	}

	if r.srNewest != nil {
		srNewestCopy := *r.srNewest
		srNewest = &srNewestCopy
	}
	return
}

func (r *RTPStats) GetExpectedRTPTimestamp(at time.Time) (expectedTSExt uint64, err error) {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if !r.initialized {
		err = errors.New("uninitilaized")
		return
	}

	timeDiff := at.Sub(r.firstTime)
	expectedRTPDiff := timeDiff.Nanoseconds() * int64(r.params.ClockRate) / 1e9
	expectedTSExt = r.timestamp.GetExtendedStart() + uint64(expectedRTPDiff)
	return
}

func (r *RTPStats) GetRtcpSenderReport(ssrc uint32, calculatedClockRate uint32) *rtcp.SenderReport {
	r.lock.Lock()
	defer r.lock.Unlock()

	if !r.initialized {
		return nil
	}

	// construct current time based on monotonic clock
	timeSinceFirst := time.Since(r.firstTime)
	now := r.firstTime.Add(timeSinceFirst)
	nowNTP := mediatransportutil.ToNtpTime(now)

	timeSinceHighest := now.Sub(r.highestTime)
	nowRTPExt := r.timestamp.GetExtendedHighest() + uint64(timeSinceHighest.Nanoseconds()*int64(r.params.ClockRate)/1e9)
	nowRTPExtUsingTime := nowRTPExt
	nowRTP := uint32(nowRTPExt)

	// It is possible that publisher is pacing at a slower rate.
	// That would make `highestTS` to be lagging the RTP time stamp in the RTCP Sender Report from publisher.
	// Check for that using calculated clock rate and use the later time stamp if applicable.
	var nowRTPExtUsingRate uint64
	if calculatedClockRate != 0 {
		nowRTPExtUsingRate = r.timestamp.GetExtendedStart() + uint64(float64(calculatedClockRate)*timeSinceFirst.Seconds())
		if nowRTPExtUsingRate > nowRTPExt {
			nowRTPExt = nowRTPExtUsingRate
			nowRTP = uint32(nowRTPExt)
		}
	}

	if r.srNewest != nil && nowRTPExt < r.srNewest.RTPTimestampExt {
		// If report being generated is behind, use the time difference and
		// clock rate of codec to produce next report.
		//
		// Current report could be behind due to the following
		//  - Publisher pacing
		//  - Due to above, report from publisher side is ahead of packet timestamps.
		//    Note that report will map wall clock to timestamp at capture time and happens before the pacer.
		//  - Pause/Mute followed by resume, some combination of events that could
		//    result in this module not having calculated clock rate of publisher side.
		//  - When the above happens, current will be generated using highestTS which could be behind.
		//    That could end up behind the last report's timestamp in extreme cases
		r.logger.Infow(
			"sending sender report, out-of-order, repairing",
			"prevTSExt", r.srNewest.RTPTimestampExt,
			"prevRTP", r.srNewest.RTPTimestamp,
			"prevNTP", r.srNewest.NTPTimestamp.Time().String(),
			"currTSExt", nowRTPExt,
			"currRTP", nowRTP,
			"currNTP", nowNTP.Time().String(),
			"timeNow", time.Now().String(),
			"firstTime", r.firstTime.String(),
			"timeSinceFirst", timeSinceFirst,
			"highestTime", r.highestTime.String(),
			"timeSinceHighest", timeSinceHighest,
			"nowRTPExtUsingTime", nowRTPExtUsingTime,
			"calculatedClockRate", calculatedClockRate,
			"nowRTPExtUsingRate", nowRTPExtUsingRate,
		)
		ntpDiffSinceLast := nowNTP.Time().Sub(r.srNewest.NTPTimestamp.Time())
		nowRTPExt = r.srNewest.RTPTimestampExt + uint64(ntpDiffSinceLast.Seconds()*float64(r.params.ClockRate))
		nowRTP = uint32(nowRTPExt)
	}

	r.srNewest = &RTCPSenderReportData{
		NTPTimestamp:    nowNTP,
		RTPTimestamp:    nowRTP,
		RTPTimestampExt: nowRTPExt,
		At:              now,
	}
	if r.srFirst == nil {
		r.srFirst = r.srNewest
	}

	return &rtcp.SenderReport{
		SSRC:        ssrc,
		NTPTime:     uint64(nowNTP),
		RTPTime:     nowRTP,
		PacketCount: uint32(r.getTotalPacketsPrimary() + r.packetsDuplicate + r.packetsPadding),
		OctetCount:  uint32(r.bytes + r.bytesDuplicate + r.bytesPadding),
	}
}

func (r *RTPStats) SnapshotRtcpReceptionReport(ssrc uint32, proxyFracLost uint8, snapshotId uint32) *rtcp.ReceptionReport {
	r.lock.Lock()
	then, now := r.getAndResetSnapshot(snapshotId, false)
	r.lock.Unlock()

	if now == nil || then == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	packetsExpected := now.extStartSN - then.extStartSN
	if packetsExpected > NumSequenceNumbers {
		r.logger.Warnw(
			"too many packets expected in receiver report",
			fmt.Errorf("start: %d, end: %d, expected: %d", then.extStartSN, now.extStartSN, packetsExpected),
		)
		return nil
	}
	if packetsExpected == 0 {
		return nil
	}

	intervalStats := r.getIntervalStats(then.extStartSN, now.extStartSN)
	packetsLost := intervalStats.packetsLost
	lossRate := float32(packetsLost) / float32(packetsExpected)
	fracLost := uint8(lossRate * 256.0)
	if proxyFracLost > fracLost {
		fracLost = proxyFracLost
	}

	var dlsr uint32
	if r.srNewest != nil && !r.srNewest.At.IsZero() {
		delayMS := uint32(time.Since(r.srNewest.At).Milliseconds())
		dlsr = (delayMS / 1e3) << 16
		dlsr |= (delayMS % 1e3) * 65536 / 1000
	}

	lastSR := uint32(0)
	if r.srNewest != nil {
		lastSR = uint32(r.srNewest.NTPTimestamp >> 16)
	}
	return &rtcp.ReceptionReport{
		SSRC:               ssrc,
		FractionLost:       fracLost,
		TotalLost:          uint32(r.packetsLost),
		LastSequenceNumber: uint32(now.extStartSN),
		Jitter:             uint32(r.jitter),
		LastSenderReport:   lastSR,
		Delay:              dlsr,
	}
}

func (r *RTPStats) DeltaInfo(snapshotId uint32) *RTPDeltaInfo {
	r.lock.Lock()
	then, now := r.getAndResetSnapshot(snapshotId, false)
	r.lock.Unlock()

	if now == nil || then == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := now.extStartSN - then.extStartSN
	if packetsExpected > NumSequenceNumbers {
		r.logger.Errorw(
			"too many packets expected in delta",
			fmt.Errorf("start: %d, end: %d, expected: %d", then.extStartSN, now.extStartSN, packetsExpected),
		)
		return nil
	}
	if packetsExpected == 0 {
		return &RTPDeltaInfo{
			StartTime: startTime,
			Duration:  endTime.Sub(startTime),
		}
	}

	intervalStats := r.getIntervalStats(then.extStartSN, now.extStartSN)
	return &RTPDeltaInfo{
		StartTime:            startTime,
		Duration:             endTime.Sub(startTime),
		Packets:              uint32(packetsExpected - intervalStats.packetsPadding),
		Bytes:                intervalStats.bytes,
		HeaderBytes:          intervalStats.headerBytes,
		PacketsDuplicate:     uint32(now.packetsDuplicate - then.packetsDuplicate),
		BytesDuplicate:       now.bytesDuplicate - then.bytesDuplicate,
		HeaderBytesDuplicate: now.headerBytesDuplicate - then.headerBytesDuplicate,
		PacketsPadding:       uint32(intervalStats.packetsPadding),
		BytesPadding:         intervalStats.bytesPadding,
		HeaderBytesPadding:   intervalStats.headerBytesPadding,
		PacketsLost:          uint32(intervalStats.packetsLost),
		Frames:               intervalStats.frames,
		RttMax:               then.maxRtt,
		JitterMax:            then.maxJitter / float64(r.params.ClockRate) * 1e6,
		Nacks:                now.nacks - then.nacks,
		Plis:                 now.plis - then.plis,
		Firs:                 now.firs - then.firs,
	}
}

func (r *RTPStats) DeltaInfoOverridden(snapshotId uint32) *RTPDeltaInfo {
	if !r.params.IsReceiverReportDriven {
		return nil
	}

	r.lock.Lock()
	then, now := r.getAndResetSnapshot(snapshotId, true)
	r.lock.Unlock()

	if now == nil || then == nil {
		return nil
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	startTime := then.startTime
	endTime := now.startTime

	packetsExpected := now.extStartSNOverridden - then.extStartSNOverridden
	if packetsExpected > NumSequenceNumbers {
		r.logger.Warnw(
			"too many packets expected in delta (overridden)",
			fmt.Errorf("start: %d, end: %d, expected: %d", then.extStartSNOverridden, now.extStartSNOverridden, packetsExpected),
		)
		return nil
	}
	if packetsExpected == 0 {
		// not received RTCP RR (OR) publisher is not producing any data
		return nil
	}

	intervalStats := r.getIntervalStats(then.extStartSNOverridden, now.extStartSNOverridden)
	packetsLost := now.packetsLostOverridden - then.packetsLostOverridden
	if int32(packetsLost) < 0 {
		packetsLost = 0
	}

	if packetsLost > packetsExpected {
		r.logger.Warnw(
			"unexpected number of packets lost",
			fmt.Errorf(
				"start: %d, end: %d, expected: %d, lost: report: %d, interval: %d",
				then.extStartSNOverridden,
				now.extStartSNOverridden,
				packetsExpected,
				now.packetsLostOverridden-then.packetsLostOverridden,
				intervalStats.packetsLost,
			),
		)
		packetsLost = packetsExpected
	}

	// discount jitter from publisher side + internal processing
	maxJitter := then.maxJitterOverridden - then.maxJitter
	if maxJitter < 0.0 {
		maxJitter = 0.0
	}
	maxJitterTime := maxJitter / float64(r.params.ClockRate) * 1e6

	return &RTPDeltaInfo{
		StartTime:            startTime,
		Duration:             endTime.Sub(startTime),
		Packets:              uint32(packetsExpected - intervalStats.packetsPadding),
		Bytes:                intervalStats.bytes,
		HeaderBytes:          intervalStats.headerBytes,
		PacketsDuplicate:     uint32(now.packetsDuplicate - then.packetsDuplicate),
		BytesDuplicate:       now.bytesDuplicate - then.bytesDuplicate,
		HeaderBytesDuplicate: now.headerBytesDuplicate - then.headerBytesDuplicate,
		PacketsPadding:       uint32(intervalStats.packetsPadding),
		BytesPadding:         intervalStats.bytesPadding,
		HeaderBytesPadding:   intervalStats.headerBytesPadding,
		PacketsLost:          uint32(packetsLost),
		PacketsMissing:       uint32(intervalStats.packetsLost),
		PacketsOutOfOrder:    uint32(intervalStats.packetsOutOfOrder),
		Frames:               intervalStats.frames,
		RttMax:               then.maxRtt,
		JitterMax:            maxJitterTime,
		Nacks:                now.nacks - then.nacks,
		Plis:                 now.plis - then.plis,
		Firs:                 now.firs - then.firs,
	}
}

func (r *RTPStats) ToString() string {
	p := r.ToProto()
	if p == nil {
		return ""
	}

	r.lock.RLock()
	defer r.lock.RUnlock()

	expectedPackets := r.getPacketsExpected()
	expectedPacketRate := float64(expectedPackets) / p.Duration

	str := fmt.Sprintf("t: %+v|%+v|%.2fs", p.StartTime.AsTime().Format(time.UnixDate), p.EndTime.AsTime().Format(time.UnixDate), p.Duration)

	str += fmt.Sprintf(", sn: %d|%d", r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest())
	str += fmt.Sprintf(", ep: %d|%.2f/s", expectedPackets, expectedPacketRate)

	str += fmt.Sprintf(", p: %d|%.2f/s", p.Packets, p.PacketRate)
	str += fmt.Sprintf(", l: %d|%.1f/s|%.2f%%", p.PacketsLost, p.PacketLossRate, p.PacketLossPercentage)
	str += fmt.Sprintf(", b: %d|%.1fbps|%d", p.Bytes, p.Bitrate, p.HeaderBytes)
	str += fmt.Sprintf(", f: %d|%.1f/s / %d|%+v", p.Frames, p.FrameRate, p.KeyFrames, p.LastKeyFrame.AsTime().Format(time.UnixDate))

	str += fmt.Sprintf(", d: %d|%.2f/s", p.PacketsDuplicate, p.PacketDuplicateRate)
	str += fmt.Sprintf(", bd: %d|%.1fbps|%d", p.BytesDuplicate, p.BitrateDuplicate, p.HeaderBytesDuplicate)

	str += fmt.Sprintf(", pp: %d|%.2f/s", p.PacketsPadding, p.PacketPaddingRate)
	str += fmt.Sprintf(", bp: %d|%.1fbps|%d", p.BytesPadding, p.BitratePadding, p.HeaderBytesPadding)

	str += fmt.Sprintf(", o: %d", p.PacketsOutOfOrder)

	jitter := r.jitter
	maxJitter := r.maxJitter
	if r.params.IsReceiverReportDriven {
		// NOTE: jitter includes jitter from publisher and from processing
		jitter = r.jitterOverridden
		maxJitter = r.maxJitterOverridden
	}
	str += fmt.Sprintf(", c: %d, j: %d(%.1fus)|%d(%.1fus)", r.params.ClockRate, uint32(jitter), p.JitterCurrent, uint32(maxJitter), p.JitterMax)

	if len(p.GapHistogram) != 0 {
		first := true
		str += ", gh:["
		for burst, count := range p.GapHistogram {
			if !first {
				str += ", "
			}
			first = false
			str += fmt.Sprintf("%d:%d", burst, count)
		}
		str += "]"
	}

	str += ", n:"
	str += fmt.Sprintf("%d|%d|%d|%d", p.Nacks, p.NackAcks, p.NackMisses, p.NackRepeated)

	str += ", pli:"
	str += fmt.Sprintf("%d|%+v / %d|%+v",
		p.Plis, p.LastPli.AsTime().Format(time.UnixDate),
		p.LayerLockPlis, p.LastLayerLockPli.AsTime().Format(time.UnixDate),
	)

	str += ", fir:"
	str += fmt.Sprintf("%d|%+v", p.Firs, p.LastFir.AsTime().Format(time.UnixDate))

	str += ", rtt(ms):"
	str += fmt.Sprintf("%d|%d", p.RttCurrent, p.RttMax)

	str += fmt.Sprintf(", pd: %s, rd: %s", RTPDriftToString(p.PacketDrift), RTPDriftToString(p.ReportDrift))
	return str
}

func (r *RTPStats) ToProto() *livekit.RTPStats {
	r.lock.RLock()
	defer r.lock.RUnlock()

	if r.startTime.IsZero() {
		return nil
	}

	endTime := r.endTime
	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(r.startTime).Seconds()
	if elapsed == 0.0 {
		return nil
	}

	packets := r.getTotalPacketsPrimary()
	packetRate := float64(packets) / elapsed
	bitrate := float64(r.bytes) * 8.0 / elapsed

	frameRate := float64(r.frames) / elapsed

	packetsExpected := r.getPacketsExpected()
	packetsLost := r.getPacketsLost()
	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / float32(packetsExpected) * 100.0

	packetDuplicateRate := float64(r.packetsDuplicate) / elapsed
	bitrateDuplicate := float64(r.bytesDuplicate) * 8.0 / elapsed

	packetPaddingRate := float64(r.packetsPadding) / elapsed
	bitratePadding := float64(r.bytesPadding) * 8.0 / elapsed

	jitter := r.jitter
	maxJitter := r.maxJitter
	if r.params.IsReceiverReportDriven {
		// NOTE: jitter includes jitter from publisher and from processing
		jitter = r.jitterOverridden
		maxJitter = r.maxJitterOverridden
	}
	jitterTime := jitter / float64(r.params.ClockRate) * 1e6
	maxJitterTime := maxJitter / float64(r.params.ClockRate) * 1e6

	packetDrift, reportDrift := r.getDrift()

	p := &livekit.RTPStats{
		StartTime:            timestamppb.New(r.startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              uint32(packets),
		PacketRate:           packetRate,
		Bytes:                r.bytes,
		HeaderBytes:          r.headerBytes,
		Bitrate:              bitrate,
		PacketsLost:          uint32(packetsLost),
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     uint32(r.packetsDuplicate),
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       r.bytesDuplicate,
		HeaderBytesDuplicate: r.headerBytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       uint32(r.packetsPadding),
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         r.bytesPadding,
		HeaderBytesPadding:   r.headerBytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    uint32(r.packetsOutOfOrder),
		Frames:               r.frames,
		FrameRate:            frameRate,
		KeyFrames:            r.keyFrames,
		LastKeyFrame:         timestamppb.New(r.lastKeyFrame),
		JitterCurrent:        jitterTime,
		JitterMax:            maxJitterTime,
		Nacks:                r.nacks,
		NackAcks:             r.nackAcks,
		NackMisses:           r.nackMisses,
		NackRepeated:         r.nackRepeated,
		Plis:                 r.plis,
		LastPli:              timestamppb.New(r.lastPli),
		LayerLockPlis:        r.layerLockPlis,
		LastLayerLockPli:     timestamppb.New(r.lastLayerLockPli),
		Firs:                 r.firs,
		LastFir:              timestamppb.New(r.lastFir),
		RttCurrent:           r.rtt,
		RttMax:               r.maxRtt,
		PacketDrift:          packetDrift,
		ReportDrift:          reportDrift,
	}

	gapsPresent := false
	for i := 0; i < len(r.gapHistogram); i++ {
		if r.gapHistogram[i] == 0 {
			continue
		}

		gapsPresent = true
		break
	}

	if gapsPresent {
		p.GapHistogram = make(map[int32]uint32, GapHistogramNumBins)
		for i := 0; i < len(r.gapHistogram); i++ {
			if r.gapHistogram[i] == 0 {
				continue
			}

			p.GapHistogram[int32(i+1)] = r.gapHistogram[i]
		}
	}

	return p
}

func (r *RTPStats) getExtHighestSNAdjusted() uint64 {
	if r.params.IsReceiverReportDriven && !r.lastRRTime.IsZero() {
		return r.extHighestSNOverridden
	}

	return r.sequenceNumber.GetExtendedHighest()
}

func (r *RTPStats) getPacketsLost() uint64 {
	if r.params.IsReceiverReportDriven && !r.lastRRTime.IsZero() {
		return r.packetsLostOverridden
	}

	return r.packetsLost
}

func (r *RTPStats) getSnInfoOutOfOrderPtr(esn uint64, ehsn uint64) int {
	if int64(esn-ehsn) > 0 {
		return -1 // in-order, not expected, maybe too new
	}

	offset := ehsn - esn
	if int(offset) >= SnInfoSize {
		// too old, ignore
		return -1
	}

	return (r.snInfoWritePtr - int(offset) - 1) & SnInfoMask
}

func (r *RTPStats) setSnInfo(esn uint64, ehsn uint64, pktSize uint16, hdrSize uint16, payloadSize uint16, marker bool, isOutOfOrder bool) {
	writePtr := 0
	ooo := int64(esn-ehsn) < 0
	if !ooo {
		writePtr = r.snInfoWritePtr
		r.snInfoWritePtr = (writePtr + 1) & SnInfoMask
	} else {
		writePtr = r.getSnInfoOutOfOrderPtr(esn, ehsn)
		if writePtr < 0 {
			return
		}
	}

	snInfo := &r.snInfos[writePtr]
	snInfo.pktSize = pktSize
	snInfo.hdrSize = hdrSize
	snInfo.isPaddingOnly = payloadSize == 0
	snInfo.marker = marker
	snInfo.isOutOfOrder = isOutOfOrder
}

func (r *RTPStats) clearSnInfos(extStartInclusive uint64, extEndExclusive uint64) {
	for esn := extStartInclusive; esn != extEndExclusive; esn++ {
		snInfo := &r.snInfos[r.snInfoWritePtr]
		snInfo.pktSize = 0
		snInfo.hdrSize = 0
		snInfo.isPaddingOnly = false
		snInfo.marker = false

		r.snInfoWritePtr = (r.snInfoWritePtr + 1) & SnInfoMask
	}
}

func (r *RTPStats) isSnInfoLost(esn uint64, ehsn uint64) bool {
	readPtr := r.getSnInfoOutOfOrderPtr(esn, ehsn)
	if readPtr < 0 {
		return false
	}

	snInfo := &r.snInfos[readPtr]
	return snInfo.pktSize == 0
}

func (r *RTPStats) getIntervalStats(extStartInclusive uint64, extEndExclusive uint64) (intervalStats IntervalStats) {
	packetsNotFound := uint32(0)
	processESN := func(esn uint64, ehsn uint64) {
		readPtr := r.getSnInfoOutOfOrderPtr(esn, ehsn)
		if readPtr < 0 {
			packetsNotFound++
			return
		}

		snInfo := &r.snInfos[readPtr]
		switch {
		case snInfo.pktSize == 0:
			intervalStats.packetsLost++

		case snInfo.isPaddingOnly:
			intervalStats.packetsPadding++
			intervalStats.bytesPadding += uint64(snInfo.pktSize)
			intervalStats.headerBytesPadding += uint64(snInfo.hdrSize)

		default:
			intervalStats.packets++
			intervalStats.bytes += uint64(snInfo.pktSize)
			intervalStats.headerBytes += uint64(snInfo.hdrSize)
			if snInfo.isOutOfOrder {
				intervalStats.packetsOutOfOrder++
			}
		}

		if snInfo.marker {
			intervalStats.frames++
		}
	}

	ehsn := r.sequenceNumber.GetExtendedHighest()
	for esn := extStartInclusive; esn != extEndExclusive; esn++ {
		processESN(esn, ehsn)
	}

	if packetsNotFound != 0 {
		r.logger.Errorw(
			"could not find some packets", nil,
			"start", extStartInclusive,
			"end", extEndExclusive,
			"count", packetsNotFound,
			"highestSN", r.sequenceNumber.GetExtendedHighest(),
		)
	}
	return
}

func (r *RTPStats) updateJitter(rtph *rtp.Header, packetTime time.Time) {
	// Do not update jitter on multiple packets of same frame.
	// All packets of a frame have the same time stamp.
	// NOTE: This does not protect against using more than one packet of the same frame
	//       if packets arrive out-of-order. For example,
	//          p1f1 -> p1f2 -> p2f1
	//       In this case, p2f1 (packet 2, frame 1) will still be used in jitter calculation
	//       although it is the second packet of a frame because of out-of-order receival.
	if r.lastJitterRTP == rtph.Timestamp {
		return
	}

	timeSinceFirst := packetTime.Sub(r.firstTime)
	packetTimeRTP := uint32(timeSinceFirst.Nanoseconds() * int64(r.params.ClockRate) / 1e9)
	transit := packetTimeRTP - rtph.Timestamp

	if r.lastTransit != 0 {
		d := int32(transit - r.lastTransit)
		if d < 0 {
			d = -d
		}
		r.jitter += (float64(d) - r.jitter) / 16
		if r.jitter > r.maxJitter {
			r.maxJitter = r.jitter
		}

		for _, s := range r.snapshots {
			if r.jitter > s.maxJitter {
				s.maxJitter = r.jitter
			}
		}
	}

	r.lastTransit = transit
	r.lastJitterRTP = rtph.Timestamp
}

func (r *RTPStats) getDrift() (packetDrift *livekit.RTPDrift, reportDrift *livekit.RTPDrift) {
	if !r.firstTime.IsZero() {
		elapsed := r.highestTime.Sub(r.firstTime)
		rtpClockTicks := r.timestamp.GetExtendedHighest() - r.timestamp.GetExtendedStart()
		driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
		if elapsed.Seconds() > 0.0 {
			packetDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(r.firstTime),
				EndTime:        timestamppb.New(r.highestTime),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.timestamp.GetExtendedStart(),
				EndTimestamp:   r.timestamp.GetExtendedHighest(),
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}
	}

	if r.srFirst != nil && r.srNewest != nil && r.srFirst.RTPTimestamp != r.srNewest.RTPTimestamp {
		elapsed := r.srNewest.NTPTimestamp.Time().Sub(r.srFirst.NTPTimestamp.Time())
		rtpClockTicks := r.srNewest.RTPTimestampExt - r.srFirst.RTPTimestampExt
		driftSamples := int64(rtpClockTicks - uint64(elapsed.Nanoseconds()*int64(r.params.ClockRate)/1e9))
		if elapsed.Seconds() > 0.0 {
			reportDrift = &livekit.RTPDrift{
				StartTime:      timestamppb.New(r.srFirst.NTPTimestamp.Time()),
				EndTime:        timestamppb.New(r.srNewest.NTPTimestamp.Time()),
				Duration:       elapsed.Seconds(),
				StartTimestamp: r.srFirst.RTPTimestampExt,
				EndTimestamp:   r.srNewest.RTPTimestampExt,
				RtpClockTicks:  rtpClockTicks,
				DriftSamples:   driftSamples,
				DriftMs:        (float64(driftSamples) * 1000) / float64(r.params.ClockRate),
				ClockRate:      float64(rtpClockTicks) / elapsed.Seconds(),
			}
		}
	}
	return
}

func (r *RTPStats) updateGapHistogram(gap int) {
	if gap < 2 {
		return
	}

	missing := gap - 1
	if missing > len(r.gapHistogram) {
		r.gapHistogram[len(r.gapHistogram)-1]++
	} else {
		r.gapHistogram[missing-1]++
	}
}

func (r *RTPStats) getAndResetSnapshot(snapshotId uint32, override bool) (*Snapshot, *Snapshot) {
	if !r.initialized || (override && r.lastRRTime.IsZero()) {
		return nil, nil
	}

	then := r.snapshots[snapshotId]
	if then == nil {
		extStartSN := r.sequenceNumber.GetExtendedStart()
		then = &Snapshot{
			startTime:            r.startTime,
			extStartSN:           extStartSN,
			extStartSNOverridden: extStartSN,
		}
		r.snapshots[snapshotId] = then
	}

	var startTime time.Time
	if override {
		startTime = r.lastRRTime
	} else {
		startTime = time.Now()
	}

	// snapshot now
	r.snapshots[snapshotId] = &Snapshot{
		startTime:             startTime,
		extStartSN:            r.sequenceNumber.GetExtendedHighest() + 1,
		extStartSNOverridden:  r.getExtHighestSNAdjusted() + 1,
		packetsDuplicate:      r.packetsDuplicate,
		bytesDuplicate:        r.bytesDuplicate,
		headerBytesDuplicate:  r.headerBytesDuplicate,
		packetsLostOverridden: r.packetsLostOverridden,
		nacks:                 r.nacks,
		plis:                  r.plis,
		firs:                  r.firs,
		maxJitter:             r.jitter,
		maxJitterOverridden:   r.jitterOverridden,
		maxRtt:                r.rtt,
	}
	// make a copy so that it can be used independently
	now := *r.snapshots[snapshotId]

	return then, &now
}

// ----------------------------------

func AggregateRTPStats(statsList []*livekit.RTPStats) *livekit.RTPStats {
	if len(statsList) == 0 {
		return nil
	}

	startTime := time.Time{}
	endTime := time.Time{}

	packets := uint32(0)
	bytes := uint64(0)
	headerBytes := uint64(0)
	packetsLost := uint32(0)
	packetsDuplicate := uint32(0)
	bytesDuplicate := uint64(0)
	headerBytesDuplicate := uint64(0)
	packetsPadding := uint32(0)
	bytesPadding := uint64(0)
	headerBytesPadding := uint64(0)
	packetsOutOfOrder := uint32(0)
	frames := uint32(0)
	keyFrames := uint32(0)
	lastKeyFrame := time.Time{}
	jitter := 0.0
	maxJitter := float64(0)
	gapHistogram := make(map[int32]uint32, GapHistogramNumBins)
	nacks := uint32(0)
	nackAcks := uint32(0)
	nackMisses := uint32(0)
	nackRepeated := uint32(0)
	plis := uint32(0)
	lastPli := time.Time{}
	layerLockPlis := uint32(0)
	lastLayerLockPli := time.Time{}
	firs := uint32(0)
	lastFir := time.Time{}
	rtt := uint32(0)
	maxRtt := uint32(0)

	for _, stats := range statsList {
		if startTime.IsZero() || startTime.After(stats.StartTime.AsTime()) {
			startTime = stats.StartTime.AsTime()
		}

		if endTime.IsZero() || endTime.Before(stats.EndTime.AsTime()) {
			endTime = stats.EndTime.AsTime()
		}

		packets += stats.Packets
		bytes += stats.Bytes
		headerBytes += stats.HeaderBytes

		packetsLost += stats.PacketsLost

		packetsDuplicate += stats.PacketsDuplicate
		bytesDuplicate += stats.BytesDuplicate
		headerBytesDuplicate += stats.HeaderBytesDuplicate

		packetsPadding += stats.PacketsPadding
		bytesPadding += stats.BytesPadding
		headerBytesPadding += stats.HeaderBytesPadding

		packetsOutOfOrder += stats.PacketsOutOfOrder

		frames += stats.Frames

		keyFrames += stats.KeyFrames
		if lastKeyFrame.IsZero() || lastKeyFrame.Before(stats.LastKeyFrame.AsTime()) {
			lastKeyFrame = stats.LastKeyFrame.AsTime()
		}

		jitter += stats.JitterCurrent
		if stats.JitterMax > maxJitter {
			maxJitter = stats.JitterMax
		}

		for burst, count := range stats.GapHistogram {
			gapHistogram[burst] += count
		}

		nacks += stats.Nacks
		nackAcks += stats.NackAcks
		nackMisses += stats.NackMisses
		nackRepeated += stats.NackRepeated

		plis += stats.Plis
		if lastPli.IsZero() || lastPli.Before(stats.LastPli.AsTime()) {
			lastPli = stats.LastPli.AsTime()
		}

		layerLockPlis += stats.LayerLockPlis
		if lastLayerLockPli.IsZero() || lastLayerLockPli.Before(stats.LastLayerLockPli.AsTime()) {
			lastLayerLockPli = stats.LastLayerLockPli.AsTime()
		}

		firs += stats.Firs
		if lastFir.IsZero() || lastPli.Before(stats.LastFir.AsTime()) {
			lastFir = stats.LastFir.AsTime()
		}

		rtt += stats.RttCurrent
		if stats.RttMax > maxRtt {
			maxRtt = stats.RttMax
		}
	}

	if endTime.IsZero() {
		endTime = time.Now()
	}
	elapsed := endTime.Sub(startTime).Seconds()

	packetLostRate := float64(packetsLost) / elapsed
	packetLostPercentage := float32(packetsLost) / (float32(packets) + float32(packetsLost)) * 100.0

	packetRate := float64(packets) / elapsed
	packetDuplicateRate := float64(packetsDuplicate) / elapsed
	packetPaddingRate := float64(packetsPadding) / elapsed

	bitrate := float64(bytes) * 8.0 / elapsed
	bitrateDuplicate := float64(bytesDuplicate) * 8.0 / elapsed
	bitratePadding := float64(bytesPadding) * 8.0 / elapsed

	frameRate := float64(frames) / elapsed

	return &livekit.RTPStats{
		StartTime:            timestamppb.New(startTime),
		EndTime:              timestamppb.New(endTime),
		Duration:             elapsed,
		Packets:              packets,
		PacketRate:           packetRate,
		Bytes:                bytes,
		HeaderBytes:          headerBytes,
		Bitrate:              bitrate,
		PacketsLost:          packetsLost,
		PacketLossRate:       packetLostRate,
		PacketLossPercentage: packetLostPercentage,
		PacketsDuplicate:     packetsDuplicate,
		PacketDuplicateRate:  packetDuplicateRate,
		BytesDuplicate:       bytesDuplicate,
		HeaderBytesDuplicate: headerBytesDuplicate,
		BitrateDuplicate:     bitrateDuplicate,
		PacketsPadding:       packetsPadding,
		PacketPaddingRate:    packetPaddingRate,
		BytesPadding:         bytesPadding,
		HeaderBytesPadding:   headerBytesPadding,
		BitratePadding:       bitratePadding,
		PacketsOutOfOrder:    packetsOutOfOrder,
		Frames:               frames,
		FrameRate:            frameRate,
		KeyFrames:            keyFrames,
		LastKeyFrame:         timestamppb.New(lastKeyFrame),
		JitterCurrent:        jitter / float64(len(statsList)),
		JitterMax:            maxJitter,
		GapHistogram:         gapHistogram,
		Nacks:                nacks,
		NackAcks:             nackAcks,
		NackMisses:           nackMisses,
		NackRepeated:         nackRepeated,
		Plis:                 plis,
		LastPli:              timestamppb.New(lastPli),
		LayerLockPlis:        layerLockPlis,
		LastLayerLockPli:     timestamppb.New(lastLayerLockPli),
		Firs:                 firs,
		LastFir:              timestamppb.New(lastFir),
		RttCurrent:           rtt / uint32(len(statsList)),
		RttMax:               maxRtt,
		// no aggregation for drift calculations
	}
}

func AggregateRTPDeltaInfo(deltaInfoList []*RTPDeltaInfo) *RTPDeltaInfo {
	if len(deltaInfoList) == 0 {
		return nil
	}

	startTime := time.Time{}
	endTime := time.Time{}

	packets := uint32(0)
	bytes := uint64(0)
	headerBytes := uint64(0)

	packetsDuplicate := uint32(0)
	bytesDuplicate := uint64(0)
	headerBytesDuplicate := uint64(0)

	packetsPadding := uint32(0)
	bytesPadding := uint64(0)
	headerBytesPadding := uint64(0)

	packetsLost := uint32(0)
	packetsMissing := uint32(0)
	packetsOutOfOrder := uint32(0)

	frames := uint32(0)

	maxRtt := uint32(0)
	maxJitter := float64(0)

	nacks := uint32(0)
	plis := uint32(0)
	firs := uint32(0)

	for _, deltaInfo := range deltaInfoList {
		if deltaInfo == nil {
			continue
		}

		if startTime.IsZero() || startTime.After(deltaInfo.StartTime) {
			startTime = deltaInfo.StartTime
		}

		endedAt := deltaInfo.StartTime.Add(deltaInfo.Duration)
		if endTime.IsZero() || endTime.Before(endedAt) {
			endTime = endedAt
		}

		packets += deltaInfo.Packets
		bytes += deltaInfo.Bytes
		headerBytes += deltaInfo.HeaderBytes

		packetsDuplicate += deltaInfo.PacketsDuplicate
		bytesDuplicate += deltaInfo.BytesDuplicate
		headerBytesDuplicate += deltaInfo.HeaderBytesDuplicate

		packetsPadding += deltaInfo.PacketsPadding
		bytesPadding += deltaInfo.BytesPadding
		headerBytesPadding += deltaInfo.HeaderBytesPadding

		packetsLost += deltaInfo.PacketsLost
		packetsMissing += deltaInfo.PacketsMissing
		packetsOutOfOrder += deltaInfo.PacketsOutOfOrder

		frames += deltaInfo.Frames

		if deltaInfo.RttMax > maxRtt {
			maxRtt = deltaInfo.RttMax
		}

		if deltaInfo.JitterMax > maxJitter {
			maxJitter = deltaInfo.JitterMax
		}

		nacks += deltaInfo.Nacks
		plis += deltaInfo.Plis
		firs += deltaInfo.Firs
	}
	if startTime.IsZero() || endTime.IsZero() {
		return nil
	}

	return &RTPDeltaInfo{
		StartTime:            startTime,
		Duration:             endTime.Sub(startTime),
		Packets:              packets,
		Bytes:                bytes,
		HeaderBytes:          headerBytes,
		PacketsDuplicate:     packetsDuplicate,
		BytesDuplicate:       bytesDuplicate,
		HeaderBytesDuplicate: headerBytesDuplicate,
		PacketsPadding:       packetsPadding,
		BytesPadding:         bytesPadding,
		HeaderBytesPadding:   headerBytesPadding,
		PacketsLost:          packetsLost,
		PacketsMissing:       packetsMissing,
		PacketsOutOfOrder:    packetsOutOfOrder,
		Frames:               frames,
		RttMax:               maxRtt,
		JitterMax:            maxJitter,
		Nacks:                nacks,
		Plis:                 plis,
		Firs:                 firs,
	}
}

// -------------------------------------------------------------------
