import static org.junit.Assert.assertEquals;

import org.junit.Test;

public class AppTest {
    // Compiles fine; fails at run time so -DskipTests can be shown to bypass it.
    @Test
    public void fails() {
        assertEquals(1, 2);
    }
}
