import unittest

from case_screen.ui.geometry import move_bounds, resize_bounds


class ThemeGeometryTests(unittest.TestCase):
    def test_move_uses_normalized_delta_and_keeps_size(self):
        start = {"x": 0.2, "y": 0.3, "width": 0.4, "height": 0.2}
        moved = move_bounds(start, 0.05, -0.1)
        self.assertAlmostEqual(moved["x"], 0.25)
        self.assertAlmostEqual(moved["y"], 0.2)
        self.assertEqual(moved["width"], 0.4)
        self.assertEqual(moved["height"], 0.2)

    def test_move_clamps_to_canvas(self):
        moved = move_bounds({"x": 0.8, "y": 0.8, "width": 0.3, "height": 0.3}, 0.5, 0.5)
        self.assertEqual(moved["x"], 0.7)
        self.assertEqual(moved["y"], 0.7)

    def test_resize_keeps_origin_and_clamps(self):
        resized = resize_bounds({"x": 0.2, "y": 0.1, "width": 0.4, "height": 0.3}, 0.9, 0.9)
        self.assertEqual(resized["x"], 0.2)
        self.assertEqual(resized["y"], 0.1)
        self.assertEqual(resized["width"], 0.8)
        self.assertEqual(resized["height"], 0.9)


if __name__ == "__main__":
    unittest.main()
