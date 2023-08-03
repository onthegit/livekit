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

package utils

import (
	"unsafe"
)

type number interface {
	uint16 | uint32
}

type extendedNumber interface {
	uint32 | uint64
}

type WrapAround[T number, ET extendedNumber] struct {
	fullRange ET

	initialized bool
	start       T
	highest     T
	cycles      int
}

func NewWrapAround[T number, ET extendedNumber]() *WrapAround[T, ET] {
	var t T
	return &WrapAround[T, ET]{
		fullRange: 1 << (unsafe.Sizeof(t) * 8),
	}
}

func (w *WrapAround[T, ET]) Seed(from *WrapAround[T, ET]) {
	w.initialized = from.initialized
	w.start = from.start
	w.highest = from.highest
	w.cycles = from.cycles
}

type wrapAroundUpdateResult[ET extendedNumber] struct {
	IsRestart          bool
	PreExtendedStart   ET // valid only if IsRestart = true
	PreExtendedHighest ET
	ExtendedVal        ET
}

func (w *WrapAround[T, ET]) Update(val T) (result wrapAroundUpdateResult[ET]) {
	if !w.initialized {
		result.PreExtendedHighest = ET(val) - 1
		result.ExtendedVal = ET(val)

		w.start = val
		w.highest = val
		w.initialized = true
		return
	}

	result.PreExtendedHighest = w.GetExtendedHighest()

	gap := val - w.highest
	if gap == 0 || gap > T(w.fullRange>>1) {
		// duplicate OR out-of-order
		result.IsRestart, result.PreExtendedStart, result.ExtendedVal = w.maybeAdjustStart(val)
		return
	}

	// in-order
	if val < w.highest {
		w.cycles++
	}
	w.highest = val

	result.ExtendedVal = ET(w.cycles)*w.fullRange + ET(val)
	return
}

func (w *WrapAround[T, ET]) ResetHighest(val T) {
	w.highest = val
}

func (w *WrapAround[T, ET]) GetStart() T {
	return w.start
}

func (w *WrapAround[T, ET]) GetExtendedStart() ET {
	return ET(w.start)
}

func (w *WrapAround[T, ET]) GetHighest() T {
	return w.highest
}

func (w *WrapAround[T, ET]) GetExtendedHighest() ET {
	return ET(w.cycles)*w.fullRange + ET(w.highest)
}

func (w *WrapAround[T, ET]) maybeAdjustStart(val T) (isRestart bool, preExtendedStart ET, extendedVal ET) {
	isWrapBack := func() bool {
		return ET(w.highest) < (w.fullRange>>1) && ET(val) >= (w.fullRange>>1)
	}

	// re-adjust start if necessary. The conditions are
	// 1. Not seen more than half the range yet
	// 1. wrap around compared to start and not completed a half cycle, sequences like (10, 65530) in uint16 space
	// 2. no wrap around, but out-of-order compared to start and not completed a half cycle , sequences like (10, 9), (65530, 65528) in uint16 space
	cycles := w.cycles
	totalNum := w.GetExtendedHighest() - w.GetExtendedStart() + 1
	if totalNum > (w.fullRange >> 1) {
		if isWrapBack() {
			cycles--
		}
		extendedVal = ET(cycles)*w.fullRange + ET(val)
		return
	}

	if val-w.start > T(w.fullRange>>1) {
		// out-of-order with existing start => a new start
		isRestart = true
		preExtendedStart = w.GetExtendedStart()

		if val > w.highest {
			// wrap around
			w.cycles = 1
			cycles = 0
		}
		w.start = val
	} else {
		if isWrapBack() {
			cycles--
		}
	}
	extendedVal = ET(cycles)*w.fullRange + ET(val)
	return
}
