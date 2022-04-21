#include "sortition.h"
#include <boost/math/distributions/binomial.hpp>

uint64_t sortition_binomial_cdf_walk(double n, double p, double ratio, uint64_t money) {
  boost::math::binomial_distribution<double> dist(n, p);
  for (uint64_t j = 0; j < money; j++) {
    // Get the cdf
    double boundary = cdf(dist, j);

    // Found the correct boundary, break
    if (ratio <= boundary) {
      return j;
    }
 //   printf("The _추첨 j_ %lld", j);
//    printf("_boundary_ %lf", boundary);
//    printf("_ratio_ %lf ", ratio);
//    printf("_dist_ %lf ", dist);
//    printf("_머니_ %lf _expected/total_ %lf.\n", n,p);
    //printf("The _추첨 j_ $d _boundary_ $lf _ratio_ $lf _dist_ $lf _머니_ $lf _expected/total_ %lf.\n", j, boundary,ratio,dist,n,p);
    //printf("_추첨 j_",j,"_boundary_",boundary,"_ratio_",ratio,"_dist_",dist,"머니",n,"expected/total",p)
  }
  return money;
}
