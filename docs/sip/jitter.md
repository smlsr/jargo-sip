# Jitter buffer

Owner: JAR-6. The buffer is our sequence ring. Pion's interceptor jitter buffer is not used.

`JitterConfig` zeros mean Target 40 ms, Min 20 ms, Max 200 ms. The values are multiples of the 20 ms frame. A value that is not a multiple of 20 ms is rounded up to the next 20 ms frame at `Start`, and Min is kept less than or equal to Target, which is kept less than or equal to Max.

Target is how long the playout head waits behind the newest packet. The playout ticker is 20 ms. Playout starts after Target of audio is buffered, or when the oldest packet is older than Target. With Target 60 ms and packets arriving on time, playout does not start early. That is a JAR-6 check.

A gap repeats the previous frame once, counts one underrun, and does not wait for the missing packet. `ReadPCM` returns that concealment frame and a nil error. A dropped packet is one concealment frame.

Telephone-event is demuxed before decode. It consumes the sequence number and is not loss. The playout tick conceals that 20 ms so the audio clock stays aligned.

Late packets are dropped. They do not move the playout head backward.

Every 2 s, if the depth sat above Target+20 ms for that whole window and there was no underrun, lower Target by 20 ms, down to Min. On an underrun, raise Target by 20 ms, up to Max. The next playout uses the new target.

Reset the buffer when the remote address changes: a latch switch, or a re-INVITE or UPDATE that changes `c=` or `m=`. Packets from the old address are discarded.

`ReadPCM` returns one 20 ms frame per call, in playout order. It blocks until the first frame is due or the call is done. After `Done` is closed, it returns an error.
