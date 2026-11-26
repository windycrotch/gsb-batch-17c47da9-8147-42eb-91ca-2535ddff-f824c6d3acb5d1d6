//! Disconnect/reconnect recovery shared by the client input sender and prediction buffer.
//!
//! A reconnect handshake carries the server's cumulative input acknowledgement and the Tick of its
//! newest authoritative state. [`recover_connection`] restores both client components
//! transactionally from one handshake: every validation runs before any state changes, so a refusal
//! (an impossible acknowledgement, a backwards authoritative Tick, or a replay set beyond the
//! configured limit) leaves both the sender and the prediction buffer exactly as they were, letting
//! the caller perform a fresh handshake.

use serde::{Deserialize, Serialize};

use crate::NetcodeResult;
use crate::input::InputSender;
use crate::prediction::PredictionBuffer;

/// Server progress offered to a client during a reconnect handshake.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct ReconnectHandshake {
    /// Highest contiguous client input sequence cumulatively received by the server.
    pub acknowledged_sequence: u64,
    /// Tick of the newest authoritative server state the client can restore from.
    pub authoritative_server_tick: u64,
}

/// Outcome of a successful transactional reconnect.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct ReconnectReport {
    /// Cumulative acknowledgement the client recovered to.
    pub acknowledged_sequence: u64,
    /// Authoritative Tick the client recovered to.
    pub authoritative_server_tick: u64,
    /// Confirmed inputs released from the sender history.
    pub sender_inputs_released: usize,
    /// Confirmed prediction frames discarded.
    pub prediction_frames_discarded: usize,
    /// Local inputs retained for replay after the authoritative Tick.
    pub prediction_inputs_retained: usize,
}

/// Restores an input sender and prediction buffer together from one handshake.
///
/// Both components are validated before either is mutated, so the recovery is all-or-nothing:
///
/// * a handshake acknowledgement beyond the largest issued sequence is refused and nothing changes;
/// * an authoritative Tick earlier than the local confirmed Tick is refused and nothing changes;
/// * when replaying the inputs after the authoritative Tick would exceed the prediction replay
///   limit, the sender is not allowed to drop any input and both components keep their old state.
///
/// On success the sender retains only genuinely unacknowledged inputs (later packets therefore
/// carry just those), while the prediction buffer retains only local inputs recorded strictly after
/// the authoritative Tick. Input numbering is untouched, so the next recorded input continues from
/// the previous sequence rather than restarting at one.
pub fn recover_connection<T, I, S>(
    sender: &mut InputSender<T>,
    prediction: &mut PredictionBuffer<I, S>,
    handshake: ReconnectHandshake,
) -> NetcodeResult<ReconnectReport> {
    // Validate both components up front. A failure returns before any mutation, preserving the
    // original state of both the sender and the prediction buffer.
    let sender_inputs_released = sender.check_reconnect(
        handshake.acknowledged_sequence,
        handshake.authoritative_server_tick,
    )?;
    let prediction_inputs_retained =
        prediction.check_reconnect(handshake.authoritative_server_tick)?;

    // Both checks passed: commit. The sender drops inputs only after the prediction buffer has
    // accepted the recovery, so the two always advance together or not at all.
    let released = sender.commit_reconnect(
        handshake.acknowledged_sequence,
        handshake.authoritative_server_tick,
    );
    debug_assert_eq!(released, sender_inputs_released);
    let discarded = prediction.commit_reconnect(handshake.authoritative_server_tick);

    Ok(ReconnectReport {
        acknowledged_sequence: sender.acknowledged_sequence(),
        authoritative_server_tick: handshake.authoritative_server_tick,
        sender_inputs_released: released,
        prediction_frames_discarded: discarded,
        prediction_inputs_retained,
    })
}
