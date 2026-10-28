import java.lang.reflect.Method;

/**
 * Sets the station's hardware (amplifier) volume through the Amlogic audio HAL:
 * AudioSystem.setParameters("hw_master_volume=N"), N in 0..100. The HAL then
 * writes the TAS5825 "Tweeters DSP Playback Volume" mixer control.
 * Prints the value read back. Run as root via app_process (see hwvol.sh).
 */
public class HwVol {
    public static void main(String[] args) throws Exception {
        if (args.length != 1) {
            System.err.println("usage: HwVol <0-100>");
            System.exit(2);
        }
        int v = Integer.parseInt(args[0]);
        if (v < 0 || v > 100) {
            System.err.println("volume out of range: " + v);
            System.exit(2);
        }
        Class<?> as = Class.forName("android.media.AudioSystem");
        Method set = as.getMethod("setParameters", String.class);
        Method get = as.getMethod("getParameters", String.class);
        int rc = (Integer) set.invoke(null, "hw_master_volume=" + v);
        String back = (String) get.invoke(null, "hw_master_volume");
        System.out.println(back);
        if (rc != 0 || !back.equals("hw_master_volume=" + v)) {
            System.err.println("setParameters rc=" + rc);
            System.exit(1);
        }
    }
}
