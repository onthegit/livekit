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
	"fmt"
	"math/rand"
	"testing"
	"time"

	"github.com/livekit/protocol/logger"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/require"
)

func getPacket(sn uint16, ts uint32, payloadSize int) *rtp.Packet {
	return &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: sn,
			Timestamp:      ts,
		},
		Payload: make([]byte, payloadSize),
	}
}

func TestRTPStats(t *testing.T) {
	clockRate := uint32(90000)
	r := NewRTPStats(RTPStatsParams{
		ClockRate: clockRate,
		Logger:    logger.GetLogger(),
	})

	totalDuration := 5 * time.Second
	bitrate := 1000000
	packetSize := 1000
	pps := (((bitrate + 7) / 8) + packetSize - 1) / packetSize
	framerate := 30
	sleep := 1000 / framerate
	packetsPerFrame := (pps + framerate - 1) / framerate

	sequenceNumber := uint16(rand.Float64() * float64(1<<16))
	timestamp := uint32(rand.Float64() * float64(1<<32))
	now := time.Now()
	startTime := now
	lastFrameTime := now
	for now.Sub(startTime) < totalDuration {
		timestamp += uint32(now.Sub(lastFrameTime).Seconds() * float64(clockRate))
		for i := 0; i < packetsPerFrame; i++ {
			packet := getPacket(sequenceNumber, timestamp, packetSize)
			r.Update(&packet.Header, len(packet.Payload), 0, time.Now())
			if (sequenceNumber % 100) == 0 {
				jump := uint16(rand.Float64() * 120.0)
				sequenceNumber += jump
			} else {
				sequenceNumber++
			}
		}

		lastFrameTime = now
		time.Sleep(time.Duration(sleep) * time.Millisecond)
		now = time.Now()
	}

	r.Stop()
	fmt.Printf("%s\n", r.ToString())
}

func TestRTPStats_Update(t *testing.T) {
	clockRate := uint32(90000)
	r := NewRTPStats(RTPStatsParams{
		ClockRate: clockRate,
		Logger:    logger.GetLogger(),
	})

	sequenceNumber := uint16(rand.Float64() * float64(1<<16))
	timestamp := uint32(rand.Float64() * float64(1<<32))
	packet := getPacket(sequenceNumber, timestamp, 1000)
	flowState := r.Update(&packet.Header, len(packet.Payload), 0, time.Now())
	require.False(t, flowState.HasLoss)
	require.True(t, r.initialized)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	// in-order, no loss
	sequenceNumber++
	timestamp += 3000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(&packet.Header, len(packet.Payload), 0, time.Now())
	require.False(t, flowState.HasLoss)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))

	// out-of-order
	packet = getPacket(sequenceNumber-10, timestamp-30000, 1000)
	flowState = r.Update(&packet.Header, len(packet.Payload), 0, time.Now())
	require.False(t, flowState.HasLoss)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(1), r.packetsOutOfOrder)
	require.Equal(t, uint64(0), r.packetsDuplicate)

	// duplicate
	packet = getPacket(sequenceNumber-10, timestamp-30000, 1000)
	flowState = r.Update(&packet.Header, len(packet.Payload), 0, time.Now())
	require.False(t, flowState.HasLoss)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(2), r.packetsOutOfOrder)
	require.Equal(t, uint64(1), r.packetsDuplicate)

	// loss
	sequenceNumber += 10
	timestamp += 30000
	packet = getPacket(sequenceNumber, timestamp, 1000)
	flowState = r.Update(&packet.Header, len(packet.Payload), 0, time.Now())
	require.True(t, flowState.HasLoss)
	require.Equal(t, uint64(sequenceNumber-9), flowState.LossStartInclusive)
	require.Equal(t, uint64(sequenceNumber), flowState.LossEndExclusive)
	require.Equal(t, uint64(17), r.packetsLost)

	// out-of-order should decrement number of lost packets
	packet = getPacket(sequenceNumber-15, timestamp-45000, 1000)
	flowState = r.Update(&packet.Header, len(packet.Payload), 0, time.Now())
	require.False(t, flowState.HasLoss)
	require.Equal(t, sequenceNumber, r.sequenceNumber.GetHighest())
	require.Equal(t, sequenceNumber, uint16(r.sequenceNumber.GetExtendedHighest()))
	require.Equal(t, timestamp, r.timestamp.GetHighest())
	require.Equal(t, timestamp, uint32(r.timestamp.GetExtendedHighest()))
	require.Equal(t, uint64(3), r.packetsOutOfOrder)
	require.Equal(t, uint64(1), r.packetsDuplicate)
	require.Equal(t, uint64(16), r.packetsLost)
	intervalStats := r.getIntervalStats(r.sequenceNumber.GetExtendedStart(), r.sequenceNumber.GetExtendedHighest()+1)
	require.Equal(t, uint64(16), intervalStats.packetsLost)

	r.Stop()
}
