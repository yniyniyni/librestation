// Streams the raw mic array to stdout: interleaved int16 LE, 16 channels, 48 kHz,
// in blocks of 960 frames (20 ms). First stderr line: "mic: started session=N".
// Run: CLASSPATH=mic.dex app_process /system/bin Mic
import android.media.AudioFormat;
import android.media.AudioRecord;
import android.os.Looper;
import java.io.FileDescriptor;
import java.io.FileOutputStream;
import java.io.OutputStream;

public class Mic {
    static final int RATE = 48000, CH = 16, FRAMES = 960;

    public static void main(String[] args) throws Exception {
        Looper.prepareMainLooper();
        // AudioRecord needs a package name from ActivityThread; bare app_process has none.
        Class.forName("android.app.ActivityThread").getMethod("systemMain").invoke(null);
        AudioFormat fmt = new AudioFormat.Builder()
                .setEncoding(AudioFormat.ENCODING_PCM_16BIT)
                .setSampleRate(RATE)
                .setChannelIndexMask(0xffff)
                .build();
        int min = AudioRecord.getMinBufferSize(RATE, AudioFormat.CHANNEL_IN_MONO, AudioFormat.ENCODING_PCM_16BIT);
        int block = FRAMES * CH * 2;
        AudioRecord rec = new AudioRecord.Builder()
                .setAudioSource(1)
                .setAudioFormat(fmt)
                .setBufferSizeInBytes(Math.max(min, block * 8))
                .build();
        if (rec.getState() != AudioRecord.STATE_INITIALIZED) {
            System.err.println("mic: AudioRecord not initialized state=" + rec.getState());
            System.exit(2);
        }
        rec.startRecording();
        if (rec.getRecordingState() != AudioRecord.RECORDSTATE_RECORDING) {
            System.err.println("mic: not recording state=" + rec.getRecordingState());
            System.exit(2);
        }
        System.err.println("mic: started session=" + rec.getAudioSessionId());
        System.err.flush();

        // Raw fd, not System.out: PrintStream swallows EPIPE, so we would never notice a dead reader.
        OutputStream out = new FileOutputStream(FileDescriptor.out);
        byte[] buf = new byte[block];
        while (true) {
            int got = 0;
            while (got < block) {
                int n = rec.read(buf, got, block - got);
                if (n < 0) {
                    System.err.println("mic: read error " + n);
                    System.exit(1);
                }
                got += n;
            }
            out.write(buf, 0, block);
        }
    }
}
