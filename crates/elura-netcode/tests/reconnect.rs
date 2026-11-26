use elura_netcode::{
    InputFrame, InputPacket, InputReceiver, InputReceiverConfig, InputSender, InputSenderConfig,
    NetcodeError, PredictionBuffer, PredictionConfig, ReconnectHandshake, recover_connection,
};

/// Builds a client packet carrying the given input frames.
fn packet(inputs: Vec<InputFrame<i32>>, acknowledged_server_tick: u64) -> InputPacket<i32> {
    InputPacket {
        client_tick: 0,
        acknowledged_server_tick,
        inputs,
    }
}

/// Sender and the inputs it has recorded, keyed by sequence for easy rebuilding.
fn sender_with_inputs(history_capacity: usize, count: u64) -> InputSender<i32> {
    let mut config = InputSenderConfig::default();
    config.history_capacity = history_capacity;
    config.redundancy = history_capacity;
    let mut sender = InputSender::new(config).unwrap();
    for tick in 1..=count {
        sender.record(tick, tick as i32).unwrap();
    }
    sender
}

/// Prediction buffer holding frames on Ticks 1..=count.
fn prediction_with_frames(config: PredictionConfig, count: u64) -> PredictionBuffer<i32, i32> {
    let mut prediction = PredictionBuffer::new(config).unwrap();
    for tick in 1..=count {
        prediction.record(tick, tick as i32, tick as i32).unwrap();
    }
    prediction
}

#[test]
fn normal_reconnect_resumes_from_last_acknowledged_progress() {
    // Six inputs issued; the server cumulatively confirmed the first four before the disconnect.
    let mut sender = sender_with_inputs(16, 6);
    let mut prediction = prediction_with_frames(PredictionConfig::default(), 6);

    let handshake = ReconnectHandshake {
        acknowledged_sequence: 4,
        authoritative_server_tick: 4,
    };
    let report = recover_connection(&mut sender, &mut prediction, handshake).unwrap();

    // The sender keeps only the genuinely unacknowledged inputs (sequences five and six).
    assert_eq!(report.sender_inputs_released, 4);
    assert_eq!(report.prediction_frames_discarded, 4);
    assert_eq!(report.prediction_inputs_retained, 2);
    assert_eq!(sender.acknowledged_sequence(), 4);
    assert_eq!(sender.last_issued_sequence(), 6);
    assert_eq!(sender.pending_len(), 2);
    assert_eq!(
        sender
            .packet(7)
            .inputs
            .iter()
            .map(|frame| frame.sequence)
            .collect::<Vec<_>>(),
        vec![5, 6]
    );

    // The prediction buffer only replays local inputs strictly after the authoritative Tick.
    assert_eq!(prediction.confirmed_tick(), 4);
    assert_eq!(prediction.pending_len(), 2);
    assert_eq!(
        prediction
            .frames()
            .map(|frame| frame.tick)
            .collect::<Vec<_>>(),
        vec![5, 6]
    );

    // Numbering continues rather than restarting at sequence one.
    let next = sender.record(7, 7).unwrap();
    assert_eq!(next, 7);
    assert_eq!(
        sender
            .packet(8)
            .inputs
            .iter()
            .map(|frame| frame.sequence)
            .collect::<Vec<_>>(),
        vec![5, 6, 7]
    );
}

#[test]
fn invalid_handshake_is_rejected_and_leaves_everything_unchanged() {
    let assert_unchanged = |sender: &InputSender<i32>, prediction: &PredictionBuffer<i32, i32>| {
        assert_eq!(sender.acknowledged_sequence(), 0);
        assert_eq!(sender.last_issued_sequence(), 4);
        assert_eq!(sender.pending_len(), 4);
        assert_eq!(sender.acknowledged_server_tick(), 0);
        assert_eq!(prediction.confirmed_tick(), 0);
        assert_eq!(prediction.pending_len(), 4);
    };

    // Case 1: the handshake acknowledges beyond the largest sequence the client ever issued.
    let mut sender = sender_with_inputs(16, 4);
    let mut prediction = prediction_with_frames(PredictionConfig::default(), 4);
    let result = recover_connection(
        &mut sender,
        &mut prediction,
        ReconnectHandshake {
            acknowledged_sequence: 99,
            authoritative_server_tick: 2,
        },
    );
    assert_eq!(result, Err(NetcodeError::InvalidAcknowledgement));
    assert_unchanged(&sender, &prediction);

    // Case 2: the authoritative Tick is earlier than the local already-confirmed Tick.
    prediction.reset(3);
    let result = recover_connection(
        &mut sender,
        &mut prediction,
        ReconnectHandshake {
            acknowledged_sequence: 2,
            authoritative_server_tick: 1,
        },
    );
    assert!(matches!(result, Err(NetcodeError::InvalidInput(_))));
    // Sender untouched by the failed recovery and prediction keeps its confirmed Tick of three.
    assert_eq!(sender.acknowledged_sequence(), 0);
    assert_eq!(sender.pending_len(), 4);
    assert_eq!(prediction.confirmed_tick(), 3);

    // The component-level sender recovery independently rejects the same impossible handshake.
    let mut standalone_sender = sender_with_inputs(16, 4);
    assert_eq!(
        standalone_sender.reconnect(99, 2),
        Err(NetcodeError::InvalidAcknowledgement)
    );
    assert_eq!(standalone_sender.pending_len(), 4);
}

#[test]
fn in_flight_old_packets_after_reconnect_are_duplicates_and_ack_never_regresses() {
    let mut receiver = InputReceiver::new(InputReceiverConfig::default()).unwrap();

    // Before the disconnect the server had cumulatively received sequences one through four.
    let pre_disconnect: Vec<InputFrame<i32>> = (1..=4)
        .map(|sequence| InputFrame {
            sequence,
            target_tick: 10 + sequence,
            input: sequence as i32,
        })
        .collect();
    let first = receiver.receive(14, packet(pre_disconnect, 0)).unwrap();
    assert_eq!(first.acknowledgement.acknowledged_sequence, 4);

    // Reconnect handshake is built from the receiver's preserved high-water mark.
    let handshake = receiver.reconnect_handshake(15);
    assert_eq!(
        handshake,
        ReconnectHandshake {
            acknowledged_sequence: 4,
            authoritative_server_tick: 15,
        }
    );

    // A packet that was already in flight before the disconnect arrives after the reconnect.
    let stale: Vec<InputFrame<i32>> = (1..=4)
        .map(|sequence| InputFrame {
            sequence,
            target_tick: 10 + sequence,
            input: sequence as i32,
        })
        .collect();
    let stale_report = receiver.receive(16, packet(stale, 15)).unwrap();
    assert!(stale_report.accepted.is_empty());
    assert_eq!(stale_report.duplicates, 4);
    // The cumulative acknowledgement holds rather than moving backwards.
    assert_eq!(stale_report.acknowledgement.acknowledged_sequence, 4);
    assert_eq!(receiver.acknowledged_sequence(), 4);

    // New inputs after the reconnect advance the cumulative acknowledgement normally.
    let fresh = vec![
        InputFrame {
            sequence: 5,
            target_tick: 17,
            input: 5,
        },
        InputFrame {
            sequence: 6,
            target_tick: 18,
            input: 6,
        },
    ];
    let fresh_report = receiver.receive(17, packet(fresh, 16)).unwrap();
    assert_eq!(fresh_report.accepted.len(), 2);
    assert_eq!(fresh_report.duplicates, 0);
    assert_eq!(fresh_report.acknowledgement.acknowledged_sequence, 6);
}

#[test]
fn replay_overflow_keeps_sender_and_prediction_unchanged() {
    let mut config = PredictionConfig::default();
    config.max_replay_steps = 2;
    let mut sender = sender_with_inputs(16, 5);
    let mut prediction = prediction_with_frames(config, 5);

    // Authoritative Tick two leaves three inputs (three, four, five) to replay, over the limit.
    let before_sender = (
        sender.acknowledged_sequence(),
        sender.last_issued_sequence(),
        sender.pending_len(),
    );
    let before_prediction = (prediction.confirmed_tick(), prediction.pending_len());
    let result = recover_connection(
        &mut sender,
        &mut prediction,
        ReconnectHandshake {
            acknowledged_sequence: 2,
            authoritative_server_tick: 2,
        },
    );
    assert_eq!(result, Err(NetcodeError::ReplayLimitExceeded));

    // Nothing advanced: the sender dropped no input and the buffer kept every frame.
    assert_eq!(
        (
            sender.acknowledged_sequence(),
            sender.last_issued_sequence(),
            sender.pending_len()
        ),
        before_sender
    );
    assert_eq!(
        (prediction.confirmed_tick(), prediction.pending_len()),
        before_prediction
    );
    assert_eq!(
        sender
            .packet(9)
            .inputs
            .iter()
            .map(|frame| frame.sequence)
            .collect::<Vec<_>>(),
        vec![1, 2, 3, 4, 5]
    );

    // A fresh handshake inside the replay bound then succeeds and advances both together.
    let report = recover_connection(
        &mut sender,
        &mut prediction,
        ReconnectHandshake {
            acknowledged_sequence: 4,
            authoritative_server_tick: 4,
        },
    )
    .unwrap();
    assert_eq!(report.sender_inputs_released, 4);
    assert_eq!(report.prediction_inputs_retained, 1);
    assert_eq!(sender.pending_len(), 1);
    assert_eq!(prediction.pending_len(), 1);
}

#[test]
fn full_disconnect_reconnect_converges_client_and_server_progress() {
    let mut sender_config = InputSenderConfig::default();
    sender_config.history_capacity = 32;
    sender_config.redundancy = 32;
    let mut sender = InputSender::new(sender_config).unwrap();
    let mut prediction = PredictionBuffer::new(PredictionConfig::default()).unwrap();
    let mut receiver = InputReceiver::new(InputReceiverConfig::default()).unwrap();

    // Phase 1: normal weak-network operation. Inputs one through four reach the server and are
    // cumulatively acknowledged; five and six are recorded locally but their packets are lost.
    for tick in 1..=6 {
        let sequence = sender.record(tick, tick as i32).unwrap();
        prediction.record(tick, tick as i32, tick as i32).unwrap();
        if tick <= 4 {
            let report = receiver
                .receive(
                    tick,
                    packet(
                        vec![InputFrame {
                            sequence,
                            target_tick: tick + 2,
                            input: tick as i32,
                        }],
                        0,
                    ),
                )
                .unwrap();
            sender.acknowledge(report.acknowledgement).unwrap();
        }
    }
    assert_eq!(receiver.acknowledged_sequence(), 4);
    assert_eq!(sender.acknowledged_sequence(), 4);
    assert_eq!(sender.pending_len(), 2); // sequences five and six, unacknowledged.

    // Phase 2: while disconnected, the client keeps recording local inputs the server never sees.
    for tick in 7..=9 {
        sender.record(tick, tick as i32).unwrap();
        prediction.record(tick, tick as i32, tick as i32).unwrap();
    }
    assert_eq!(sender.pending_len(), 5); // sequences five through nine.

    // Phase 3: reconnect handshake from the server's preserved acknowledgement progress.
    let handshake = receiver.reconnect_handshake(8);
    assert_eq!(handshake.acknowledged_sequence, 4);
    let report = recover_connection(&mut sender, &mut prediction, handshake).unwrap();
    assert_eq!(report.acknowledged_sequence, 4);
    assert_eq!(report.sender_inputs_released, 0);
    assert_eq!(sender.acknowledged_sequence(), 4);
    assert_eq!(sender.pending_len(), 5);
    assert_eq!(prediction.confirmed_tick(), 8);

    // Phase 4: resume normal sends; only unacknowledged inputs are transmitted.
    let resumed = sender.packet(10);
    assert_eq!(
        resumed
            .inputs
            .iter()
            .map(|frame| frame.sequence)
            .collect::<Vec<_>>(),
        vec![5, 6, 7, 8, 9]
    );
    let server_report = receiver
        .receive(10, packet(resumed.inputs.clone(), 8))
        .unwrap();
    // All five were genuinely new to the server.
    assert_eq!(server_report.accepted.len(), 5);
    assert_eq!(server_report.duplicates, 0);
    assert_eq!(server_report.acknowledgement.acknowledged_sequence, 9);

    // The client consumes the cumulative acknowledgement and reaches the same final progress.
    sender.acknowledge(server_report.acknowledgement).unwrap();
    assert!(sender.is_empty());
    assert_eq!(sender.acknowledged_sequence(), 9);
    assert_eq!(receiver.acknowledged_sequence(), 9);
}
