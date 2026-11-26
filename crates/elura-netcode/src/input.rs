use std::collections::{BTreeSet, VecDeque};

use serde::{Deserialize, Serialize};

use crate::{NetcodeError, NetcodeResult};

/// Bounds for client-side unacknowledged input history.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
#[non_exhaustive]
pub struct InputSenderConfig {
    /// Maximum inputs retained until acknowledged by the server.
    pub history_capacity: usize,
    /// Most recent unacknowledged inputs included in each packet.
    pub redundancy: usize,
}

impl Default for InputSenderConfig {
    fn default() -> Self {
        Self {
            history_capacity: 64,
            redundancy: 3,
        }
    }
}

impl InputSenderConfig {
    /// Validates sender memory and packet bounds.
    pub fn validate(&self) -> NetcodeResult<()> {
        if self.history_capacity == 0 {
            return Err(NetcodeError::InvalidConfig(
                "input history capacity must be positive",
            ));
        }
        if self.redundancy == 0 || self.redundancy > self.history_capacity {
            return Err(NetcodeError::InvalidConfig(
                "input redundancy must be within history capacity",
            ));
        }
        Ok(())
    }
}

/// Bounds for server-side input validation and reordering.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(default, deny_unknown_fields)]
#[non_exhaustive]
pub struct InputReceiverConfig {
    /// Maximum input frames accepted in one packet.
    pub max_inputs_per_packet: usize,
    /// Maximum sequence distance retained above the cumulative acknowledgement.
    pub reorder_window: u64,
    /// Maximum number of past ticks accepted for application-owned rollback.
    pub max_past_ticks: u64,
    /// Maximum number of future ticks accepted for scheduling.
    pub max_future_ticks: u64,
}

impl Default for InputReceiverConfig {
    fn default() -> Self {
        Self {
            max_inputs_per_packet: 16,
            reorder_window: 256,
            max_past_ticks: 12,
            max_future_ticks: 120,
        }
    }
}

impl InputReceiverConfig {
    /// Validates packet, sequence, and Tick bounds.
    pub fn validate(&self) -> NetcodeResult<()> {
        if self.max_inputs_per_packet == 0 {
            return Err(NetcodeError::InvalidConfig(
                "maximum inputs per packet must be positive",
            ));
        }
        if self.reorder_window == 0 {
            return Err(NetcodeError::InvalidConfig(
                "input reorder window must be positive",
            ));
        }
        if self.max_future_ticks == 0 {
            return Err(NetcodeError::InvalidConfig(
                "maximum future ticks must be positive",
            ));
        }
        Ok(())
    }
}

/// One game-specific input assigned to an authoritative simulation Tick.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct InputFrame<T> {
    /// Monotonically increasing client input sequence.
    pub sequence: u64,
    /// Authoritative server Tick on which this input should be applied.
    pub target_tick: u64,
    /// Application-owned input value.
    pub input: T,
}

/// Transport-neutral client input packet.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct InputPacket<T> {
    /// Client simulation Tick when the packet was constructed.
    pub client_tick: u64,
    /// Newest authoritative server Tick whose state the client has consumed.
    pub acknowledged_server_tick: u64,
    /// Recent unacknowledged inputs, ordered by sequence.
    pub inputs: Vec<InputFrame<T>>,
}

/// Cumulative server acknowledgement returned to one input sender.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct InputAck {
    /// Authoritative server Tick when the packet was processed.
    pub server_tick: u64,
    /// Highest contiguous client input sequence received by the server.
    pub acknowledged_sequence: u64,
}

/// Client-side bounded input history and redundant packet builder.
#[derive(Debug, Clone)]
pub struct InputSender<T> {
    config: InputSenderConfig,
    next_sequence: u64,
    acknowledged_sequence: u64,
    acknowledged_server_tick: u64,
    history: VecDeque<InputFrame<T>>,
}

impl<T> InputSender<T> {
    /// Creates an input sender whose first generated sequence is one.
    pub fn new(config: InputSenderConfig) -> NetcodeResult<Self> {
        Self::with_next_sequence(config, 1)
    }

    /// Creates an input sender with an application-restored next sequence.
    pub fn with_next_sequence(
        config: InputSenderConfig,
        next_sequence: u64,
    ) -> NetcodeResult<Self> {
        config.validate()?;
        if next_sequence == 0 {
            return Err(NetcodeError::InvalidConfig(
                "next input sequence must be positive",
            ));
        }
        Ok(Self {
            config,
            next_sequence,
            acknowledged_sequence: next_sequence - 1,
            acknowledged_server_tick: 0,
            history: VecDeque::with_capacity(config.history_capacity),
        })
    }

    /// Records one input and returns its assigned sequence.
    pub fn record(&mut self, target_tick: u64, input: T) -> NetcodeResult<u64> {
        if target_tick == 0 {
            return Err(NetcodeError::InvalidInput(
                "input target Tick must be positive",
            ));
        }
        if self.history.len() >= self.config.history_capacity {
            return Err(NetcodeError::InputHistoryFull);
        }
        let sequence = self.next_sequence;
        self.next_sequence = self
            .next_sequence
            .checked_add(1)
            .ok_or(NetcodeError::SequenceExhausted)?;
        self.history.push_back(InputFrame {
            sequence,
            target_tick,
            input,
        });
        Ok(sequence)
    }

    /// Applies a cumulative server acknowledgement and returns the number of released inputs.
    pub fn acknowledge(&mut self, acknowledgement: InputAck) -> NetcodeResult<usize> {
        let last_issued = self.next_sequence - 1;
        if acknowledgement.acknowledged_sequence > last_issued {
            return Err(NetcodeError::InvalidAcknowledgement);
        }
        self.acknowledged_server_tick = self
            .acknowledged_server_tick
            .max(acknowledgement.server_tick);
        if acknowledgement.acknowledged_sequence <= self.acknowledged_sequence {
            return Ok(0);
        }
        self.acknowledged_sequence = acknowledgement.acknowledged_sequence;
        let before = self.history.len();
        while self
            .history
            .front()
            .is_some_and(|frame| frame.sequence <= self.acknowledged_sequence)
        {
            self.history.pop_front();
        }
        Ok(before - self.history.len())
    }

    /// Returns the highest input sequence cumulatively acknowledged by the server.
    pub fn acknowledged_sequence(&self) -> u64 {
        self.acknowledged_sequence
    }

    /// Returns the highest input sequence this sender has issued so far.
    pub fn last_issued_sequence(&self) -> u64 {
        self.next_sequence - 1
    }

    /// Returns the highest authoritative server Tick observed in an acknowledgement.
    pub fn acknowledged_server_tick(&self) -> u64 {
        self.acknowledged_server_tick
    }

    /// Returns the number of unacknowledged inputs retained locally.
    pub fn pending_len(&self) -> usize {
        self.history.len()
    }

    /// Returns whether no unacknowledged inputs remain.
    pub fn is_empty(&self) -> bool {
        self.history.is_empty()
    }

    /// Validates a reconnect handshake against the issued input range without mutating state.
    ///
    /// A handshake may only acknowledge sequences the sender has actually issued. A cumulative
    /// acknowledgement behind the sender's own progress is harmless (the sender simply keeps its
    /// higher water mark), but one beyond the largest issued sequence can never be valid.
    pub(crate) fn check_reconnect(
        &self,
        acknowledged_sequence: u64,
        _authoritative_server_tick: u64,
    ) -> NetcodeResult<usize> {
        if acknowledged_sequence > self.last_issued_sequence() {
            return Err(NetcodeError::InvalidAcknowledgement);
        }
        let cutoff = self.acknowledged_sequence.max(acknowledged_sequence);
        let released = self
            .history
            .iter()
            .filter(|frame| frame.sequence <= cutoff)
            .count();
        Ok(released)
    }

    /// Commits a previously validated handshake, dropping only confirmed inputs.
    ///
    /// Inputs already confirmed before the disconnect stay discarded: the handshake acknowledgement
    /// is combined with the local high-water mark rather than resetting it, so no confirmed frame
    /// can re-enter the history.
    pub(crate) fn commit_reconnect(
        &mut self,
        acknowledged_sequence: u64,
        authoritative_server_tick: u64,
    ) -> usize {
        self.acknowledged_sequence = self.acknowledged_sequence.max(acknowledged_sequence);
        self.acknowledged_server_tick =
            self.acknowledged_server_tick.max(authoritative_server_tick);
        let before = self.history.len();
        while self
            .history
            .front()
            .is_some_and(|frame| frame.sequence <= self.acknowledged_sequence)
        {
            self.history.pop_front();
        }
        before - self.history.len()
    }

    /// Restores the sender from a reconnect handshake.
    ///
    /// On success the sender resumes from the server's acknowledged progress: its next generated
    /// sequence is unchanged (input numbering never restarts at one) and the retained history holds
    /// only inputs the server has not cumulatively acknowledged, so later packets carry only
    /// genuinely unconfirmed inputs. An impossible acknowledgement leaves the sender unchanged.
    pub fn reconnect(
        &mut self,
        acknowledged_sequence: u64,
        authoritative_server_tick: u64,
    ) -> NetcodeResult<usize> {
        self.check_reconnect(acknowledged_sequence, authoritative_server_tick)?;
        Ok(self.commit_reconnect(acknowledged_sequence, authoritative_server_tick))
    }
}

impl<T: Clone> InputSender<T> {
    /// Builds a packet containing the newest configured number of unacknowledged inputs.
    pub fn packet(&self, client_tick: u64) -> InputPacket<T> {
        let skip = self.history.len().saturating_sub(self.config.redundancy);
        InputPacket {
            client_tick,
            acknowledged_server_tick: self.acknowledged_server_tick,
            inputs: self.history.iter().skip(skip).cloned().collect(),
        }
    }
}

/// Result of observing one sequence in a [`SequenceWindow`].
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SequenceDisposition {
    /// The sequence had not previously been received.
    Accepted,
    /// The sequence was already cumulatively acknowledged or buffered.
    Duplicate,
}

/// Bounded, cumulative acknowledgement window safe for out-of-order delivery.
#[derive(Debug, Clone)]
pub struct SequenceWindow {
    reorder_window: u64,
    acknowledged: u64,
    pending: BTreeSet<u64>,
}

impl SequenceWindow {
    /// Creates a sequence window beginning before sequence one.
    pub fn new(reorder_window: u64) -> NetcodeResult<Self> {
        Self::with_acknowledged(reorder_window, 0)
    }

    /// Creates a sequence window restored from a cumulative acknowledgement.
    pub fn with_acknowledged(reorder_window: u64, acknowledged: u64) -> NetcodeResult<Self> {
        if reorder_window == 0 {
            return Err(NetcodeError::InvalidConfig(
                "input reorder window must be positive",
            ));
        }
        Ok(Self {
            reorder_window,
            acknowledged,
            pending: BTreeSet::new(),
        })
    }

    /// Observes one sequence and advances the cumulative acknowledgement when gaps close.
    pub fn observe(&mut self, sequence: u64) -> NetcodeResult<SequenceDisposition> {
        if sequence == 0 {
            return Err(NetcodeError::InvalidInput(
                "input sequence must be positive",
            ));
        }
        if sequence <= self.acknowledged || self.pending.contains(&sequence) {
            return Ok(SequenceDisposition::Duplicate);
        }
        if sequence > self.acknowledged.saturating_add(self.reorder_window) {
            return Err(NetcodeError::InvalidInput(
                "input sequence exceeds the reorder window",
            ));
        }
        self.pending.insert(sequence);
        while let Some(next) = self.acknowledged.checked_add(1) {
            if !self.pending.remove(&next) {
                break;
            }
            self.acknowledged = next;
        }
        Ok(SequenceDisposition::Accepted)
    }

    /// Returns the highest contiguous received sequence.
    pub fn acknowledged(&self) -> u64 {
        self.acknowledged
    }

    /// Returns the number of received sequences waiting above a gap.
    pub fn pending_len(&self) -> usize {
        self.pending.len()
    }
}

/// Server-side result after validating and de-duplicating one input packet.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct InputReceiveReport<T> {
    /// Newly accepted frames, ordered by input sequence.
    pub accepted: Vec<InputFrame<T>>,
    /// Number of already received frames ignored from this packet.
    pub duplicates: usize,
    /// Client simulation Tick carried by the packet.
    pub client_tick: u64,
    /// Newest server Tick acknowledged by the client.
    pub acknowledged_server_tick: u64,
    /// Cumulative input acknowledgement to return to the client.
    pub acknowledgement: InputAck,
}

/// Server-side packet validator and out-of-order de-duplicator for one client stream.
#[derive(Debug, Clone)]
pub struct InputReceiver {
    config: InputReceiverConfig,
    sequences: SequenceWindow,
}

impl InputReceiver {
    /// Creates a receiver expecting sequence one next.
    pub fn new(config: InputReceiverConfig) -> NetcodeResult<Self> {
        Self::with_acknowledged(config, 0)
    }

    /// Restores a receiver from a previously persisted cumulative acknowledgement.
    pub fn with_acknowledged(
        config: InputReceiverConfig,
        acknowledged: u64,
    ) -> NetcodeResult<Self> {
        config.validate()?;
        Ok(Self {
            sequences: SequenceWindow::with_acknowledged(config.reorder_window, acknowledged)?,
            config,
        })
    }

    /// Validates a packet at the current server Tick and returns only newly accepted inputs.
    ///
    /// Packet processing is transactional: one invalid new frame leaves the receive window
    /// unchanged. Already received redundant frames are ignored even after their target Tick ages
    /// out of the configured rollback window.
    pub fn receive<T>(
        &mut self,
        current_server_tick: u64,
        mut packet: InputPacket<T>,
    ) -> NetcodeResult<InputReceiveReport<T>> {
        if packet.inputs.len() > self.config.max_inputs_per_packet {
            return Err(NetcodeError::InvalidInput(
                "packet contains too many input frames",
            ));
        }
        if packet.acknowledged_server_tick > current_server_tick {
            return Err(NetcodeError::InvalidInput(
                "client acknowledged a future server Tick",
            ));
        }
        packet.inputs.sort_by_key(|frame| frame.sequence);
        let mut next_sequences = self.sequences.clone();
        let mut accepted = Vec::with_capacity(packet.inputs.len());
        let mut duplicates = 0;
        let earliest = current_server_tick.saturating_sub(self.config.max_past_ticks);
        let latest = current_server_tick.saturating_add(self.config.max_future_ticks);

        for frame in packet.inputs {
            match next_sequences.observe(frame.sequence)? {
                SequenceDisposition::Duplicate => duplicates += 1,
                SequenceDisposition::Accepted => {
                    if frame.target_tick == 0
                        || frame.target_tick < earliest
                        || frame.target_tick > latest
                    {
                        return Err(NetcodeError::InvalidInput(
                            "input target Tick is outside the receive window",
                        ));
                    }
                    accepted.push(frame);
                }
            }
        }

        self.sequences = next_sequences;
        Ok(InputReceiveReport {
            accepted,
            duplicates,
            client_tick: packet.client_tick,
            acknowledged_server_tick: packet.acknowledged_server_tick,
            acknowledgement: InputAck {
                server_tick: current_server_tick,
                acknowledged_sequence: self.sequences.acknowledged(),
            },
        })
    }

    /// Returns the highest contiguous received input sequence.
    pub fn acknowledged_sequence(&self) -> u64 {
        self.sequences.acknowledged()
    }

    /// Builds the reconnect handshake a server offers a client resuming this stream.
    ///
    /// The receiver keeps its existing de-duplication window, so inputs carried by in-flight packets
    /// sent before the disconnect are observed against the same cumulative acknowledgement and are
    /// classified as duplicates instead of new inputs; the cumulative acknowledgement can therefore
    /// never move backwards.
    pub fn reconnect_handshake(
        &self,
        authoritative_server_tick: u64,
    ) -> crate::reconnect::ReconnectHandshake {
        crate::reconnect::ReconnectHandshake {
            acknowledged_sequence: self.sequences.acknowledged(),
            authoritative_server_tick,
        }
    }

    /// Returns the number of out-of-order sequences waiting for a gap.
    pub fn pending_sequences(&self) -> usize {
        self.sequences.pending_len()
    }
}
