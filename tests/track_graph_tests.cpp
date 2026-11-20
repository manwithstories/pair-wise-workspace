#include "media/track_graph.hpp"

#include <catch2/catch_test_macros.hpp>

#include <string>
#include <vector>

using namespace mediaqc;

namespace {

TrackDesc track(std::string id, std::vector<std::string> deps = {}) {
  TrackDesc desc;
  desc.id = std::move(id);
  desc.kind = TrackKind::Video;
  desc.depends_on = std::move(deps);
  return desc;
}

std::vector<std::string> ordered_ids(const TrackOrder& order,
                                     const std::vector<TrackDesc>& tracks) {
  std::vector<std::string> ids;
  for (std::size_t i : order.order) ids.push_back(tracks[i].id);
  return ids;
}

}  // namespace

TEST_CASE("independent tracks keep declaration order", "[track_graph]") {
  std::vector<TrackDesc> tracks = {track("video_main"), track("audio_main"),
                                    track("ads")};
  TrackOrder order;
  OrderError error;
  REQUIRE(TrackGraph::build(tracks, order, error));
  CHECK(ordered_ids(order, tracks) ==
        std::vector<std::string>{"video_main", "audio_main", "ads"});
  CHECK(order.order.size() == 3);
}

TEST_CASE("dependencies are processed before dependants", "[track_graph]") {
  // picture depends on camera and audio; audio depends on camera.
  std::vector<TrackDesc> tracks = {
      track("picture", {"camera", "audio"}),
      track("audio", {"camera"}),
      track("camera"),
  };
  TrackOrder order;
  OrderError error;
  REQUIRE(TrackGraph::build(tracks, order, error));
  CHECK(ordered_ids(order, tracks) ==
        std::vector<std::string>{"camera", "audio", "picture"});
}

TEST_CASE("declaration order breaks ties deterministically", "[track_graph]") {
  std::vector<TrackDesc> tracks = {
      track("b", {"root"}),
      track("root"),
      track("a", {"root"}),
  };
  TrackOrder order;
  OrderError error;
  REQUIRE(TrackGraph::build(tracks, order, error));
  // After root, the ready set is {b (index 0), a (index 2)}; index order wins.
  CHECK(ordered_ids(order, tracks) == std::vector<std::string>{"root", "b", "a"});
}

TEST_CASE("cycle is rejected and names the offending tracks", "[track_graph]") {
  std::vector<TrackDesc> tracks = {
      track("a", {"c"}),
      track("b", {"a"}),
      track("c", {"b"}),
  };
  TrackOrder order;
  OrderError error;
  CHECK_FALSE(TrackGraph::build(tracks, order, error));
  CHECK(error.error.code == "dependency_cycle");
  CHECK(error.track == "a");
  CHECK(error.cycle.size() >= 3);
  CHECK(error.error.exit_code() == 2);
}

TEST_CASE("self dependency is a cycle of one", "[track_graph]") {
  std::vector<TrackDesc> tracks = {track("solo", {"solo"})};
  TrackOrder order;
  OrderError error;
  CHECK_FALSE(TrackGraph::build(tracks, order, error));
  CHECK(error.error.code == "self_dependency");
  CHECK(error.cycle == std::vector<std::string>{"solo", "solo"});
}

TEST_CASE("missing dependency names the track and its need", "[track_graph]") {
  std::vector<TrackDesc> tracks = {track("video", {"audio"})};
  TrackOrder order;
  OrderError error;
  CHECK_FALSE(TrackGraph::build(tracks, order, error));
  CHECK(error.error.code == "missing_dependency");
  CHECK(error.track == "video");
  CHECK(error.error.message.find("audio") != std::string::npos);
}

TEST_CASE("duplicate track ids are rejected", "[track_graph]") {
  std::vector<TrackDesc> tracks = {track("dup"), track("dup")};
  TrackOrder order;
  OrderError error;
  CHECK_FALSE(TrackGraph::build(tracks, order, error));
  CHECK(error.error.code == "duplicate_track");
  CHECK(error.track == "dup");
}

TEST_CASE("empty graph is rejected", "[track_graph]") {
  std::vector<TrackDesc> tracks;
  TrackOrder order;
  OrderError error;
  CHECK_FALSE(TrackGraph::build(tracks, order, error));
  CHECK(error.error.code == "no_tracks");
}
