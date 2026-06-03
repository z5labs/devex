import static org.junit.Assert.assertEquals;

import org.junit.Test;

public class AppTest {
    // Compiles fine; fails at run time so the CI test stage fails the pipeline.
    @Test
    public void fails() {
        assertEquals(1, 2);
    }
}
